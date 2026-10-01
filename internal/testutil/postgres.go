// Package testutil provides a real Postgres for integration tests.
//
// One container starts per test binary. Migrations run once into a template
// database, and every test gets its own copy via CREATE DATABASE ... TEMPLATE,
// which takes milliseconds and keeps tests fully isolated from each other.
package testutil

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"ticket/internal/db"
)

const templateDB = "ticket_template"

// PG is a running Postgres container with a migrated template database.
type PG struct {
	ctr     *postgres.PostgresContainer
	baseURL string // URL without the database name, e.g. postgres://u:p@host:port/
	admin   *pgxpool.Pool
	mu      sync.Mutex // CREATE DATABASE from one template is serialized to avoid lock errors
	n       atomic.Int64
	urls    sync.Map // *pgxpool.Pool -> URL
}

// RunWithPostgres is a TestMain helper: it starts Postgres (unless -short), runs
// the tests, and tears the container down. It returns the exit code for os.Exit.
func RunWithPostgres(m *testing.M, out **PG) int {
	flag.Parse()
	if testing.Short() {
		return m.Run()
	}
	p, err := start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "testutil: start postgres:", err)
		return 1
	}
	defer p.close()
	*out = p
	return m.Run()
}

func start(ctx context.Context) (*PG, error) {
	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase(templateDB),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, err
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}
	p := &PG{ctr: ctr, baseURL: fmt.Sprintf("postgres://test:test@%s:%s/", host, port.Port())}
	if err := db.Migrate(p.dbURL(templateDB)); err != nil {
		return nil, fmt.Errorf("migrate template: %w", err)
	}
	if p.admin, err = pgxpool.New(ctx, p.dbURL("postgres")); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PG) dbURL(name string) string { return p.baseURL + name + "?sslmode=disable" }

func (p *PG) close() {
	if p.admin != nil {
		p.admin.Close()
	}
	_ = p.ctr.Terminate(context.Background())
}

// NewDB returns a pool connected to a fresh, fully migrated database that only
// this test uses. It skips the test when integration tests are disabled (-short).
func (p *PG) NewDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if p == nil {
		t.Skip("integration test: needs Docker; run without -short")
	}
	ctx := context.Background()
	name := fmt.Sprintf("t_%d", p.n.Add(1)) // generated here, never user input
	p.mu.Lock()
	_, err := p.admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB))
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	url := p.dbURL(name)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	p.urls.Store(pool, url)
	t.Cleanup(pool.Close)
	return pool
}

// URL returns the connection URL of a database created by NewDB.
func (p *PG) URL(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	u, ok := p.urls.Load(pool)
	if !ok {
		t.Fatal("pool was not created by NewDB")
	}
	return u.(string)
}

// CreateUser inserts a user directly (bypassing password hashing) and returns its id.
func CreateUser(t *testing.T, pool *pgxpool.Pool, email, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		"INSERT INTO users (email, password_hash, role) VALUES ($1, 'x', $2) RETURNING id", email, role).Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}
