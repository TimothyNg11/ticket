// Command seed creates load-test buyers directly in Postgres and mints their
// access tokens with the deployment's JWT secret, so a load test measures the
// on-sale rather than 50,000 registrations (which are deliberately slow:
// Argon2id). It also creates one admin. For test environments only.
//
//	DATABASE_URL=... JWT_SECRET=... go run ./loadtest/seed -users 50000 -out tokens.txt
//
// Writes one access token per line to -out, and the admin's token to -admin-out.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"ticket/internal/auth"
)

func main() {
	users := flag.Int("users", 50000, "buyers to create")
	out := flag.String("out", "loadtest-tokens.txt", "file for buyer tokens")
	adminOut := flag.String("admin-out", "loadtest-admin-token.txt", "file for the admin token")
	ttl := flag.Duration("ttl", 3*time.Hour, "token lifetime (long enough for the whole test)")
	flag.Parse()
	if err := run(*users, *out, *adminOut, *ttl); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(n int, out, adminOut string, ttl time.Duration) error {
	ctx := context.Background()
	secret := []byte(os.Getenv("JWT_SECRET"))
	if len(secret) < 32 {
		return fmt.Errorf("JWT_SECRET must be the deployment's secret (>= 32 bytes)")
	}
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	issuer := auth.NewTokenIssuer(secret, ttl)
	run := uuid.NewString()[:8]

	f, err := os.Create(out) //nolint:gosec // path from the operator's own flag
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	start := time.Now()
	for done := 0; done < n; {
		batch := min(5000, n-done)
		// One statement per 5,000 users. The password hash is a placeholder: these
		// accounts only ever use the tokens minted below.
		rows, err := pool.Query(ctx, `
			INSERT INTO users (email, password_hash)
			SELECT 'load-' || $1 || '-' || g || '@example.com', 'load-test-no-login'
			FROM generate_series($2::int, $3::int) g
			RETURNING id`, run, done, done+batch-1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			tok, err := issuer.Issue(id, "user")
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(w, tok); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		done += batch
	}
	if err := w.Flush(); err != nil {
		return err
	}

	var adminID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, password_hash, role) VALUES ($1, 'load-test-no-login', 'admin') RETURNING id`,
		"load-admin-"+run+"@example.com").Scan(&adminID); err != nil {
		return err
	}
	adminTok, err := issuer.Issue(adminID, "admin")
	if err != nil {
		return err
	}
	if err := os.WriteFile(adminOut, []byte(adminTok+"\n"), 0o600); err != nil { //nolint:gosec // path is the operator's own -admin-out flag
		return err
	}
	fmt.Printf("seeded %d buyers + 1 admin in %s -> %s, %s\n", n, time.Since(start).Round(time.Millisecond), out, adminOut)
	return nil
}
