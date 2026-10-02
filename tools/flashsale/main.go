// Command flashsale runs a timed on-sale against a live deployment and checks it
// for correctness: many buyers repeatedly hold, buy, and sometimes cancel seats
// for one event, then the system's invariants are checked.
//
// Clients behave like good real clients: every write carries an Idempotency-Key,
// and transport errors or 502/503/504 are retried with the same key. That is what
// lets a buyer survive an API pod being killed mid-request (Phase 7's done-when):
// the retry either replays the stored result or runs once on a healthy pod.
//
// It exits non-zero if any order fails for a reason other than normal contention
// (seat taken, rate limited), or if any invariant is violated.
//
// The target must allow many registrations from one IP: run the API with a high
// RATE_LIMIT_SCALE (the kind and CI deployments do).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

var (
	base        = flag.String("base", envOr("BASE_URL", "http://localhost:8080"), "API base URL")
	adminEmail  = flag.String("admin-email", envOr("ADMIN_EMAIL", "admin@example.com"), "admin account (must be in ADMIN_EMAILS)")
	adminPass   = flag.String("admin-password", envOr("ADMIN_PASSWORD", "local-admin-password"), "admin password")
	users       = flag.Int("users", 200, "number of buyers")
	seats       = flag.Int("seats", 100, "seats in the event")
	duration    = flag.Duration("duration", 60*time.Second, "how long to run")
	concurrency = flag.Int("concurrency", 50, "buyers acting at once")
	cancelRate  = flag.Float64("cancel-rate", 0.1, "fraction of confirmed orders to cancel")
	insecure    = flag.Bool("insecure", false, "skip TLS verification (self-signed local ingress)")
)

var client = &http.Client{Timeout: 15 * time.Second}

// Outcome counters.
var (
	confirmed, cancelled, seatTaken, rateLimited, retries, failed atomic.Int64
	failMu                                                        sync.Mutex
	failures                                                      = map[string]int{}
)

func fail(what string) {
	failed.Add(1)
	failMu.Lock()
	failures[what]++
	failMu.Unlock()
}

func main() {
	flag.Parse()
	if *insecure {
		client.Transport = insecureTransport()
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FLASHSALE FAIL:", err)
		os.Exit(1)
	}
}

func run() error {
	admin, err := ensureLogin(*adminEmail, *adminPass)
	if err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	eventID, err := setupEvent(admin)
	if err != nil {
		return err
	}
	fmt.Printf("event %s: %d seats, %d buyers, %s\n", eventID, *seats, *users, *duration)

	tokens := make([]string, *users)
	if err := parallel(*users, 20, func(i int) error {
		tokens[i], err = ensureLogin(fmt.Sprintf("buyer-%s-%d@example.com", eventID[:8], i), "flashsale-buyer-password")
		return err
	}); err != nil {
		return fmt.Errorf("registering buyers: %w", err)
	}

	deadline := time.Now().Add(*duration)
	free := make(chan int, *users) // buyers not currently mid-purchase
	for i := range *users {
		free <- i
	}
	var wg sync.WaitGroup
	for range *concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				i := <-free
				buy(tokens[i], eventID)
				free <- i
			}
		}()
	}
	wg.Wait()

	// Give the reconciler a moment to settle anything left pending by a kill.
	time.Sleep(2 * time.Second)
	var inv map[string]int
	if _, err := request("GET", "/v1/admin/invariants", admin, nil, &inv); err != nil {
		return fmt.Errorf("invariants: %w", err)
	}
	report(inv)
	switch {
	case inv["total"] != 0:
		return fmt.Errorf("invariant violations: %v", inv)
	case failed.Load() > 0:
		return fmt.Errorf("%d failed operations: %v", failed.Load(), failures)
	case confirmed.Load() == 0:
		return errors.New("no orders confirmed: the sale never worked")
	}
	fmt.Println("FLASHSALE OK")
	return nil
}

// buy runs one purchase attempt: read the seat map, hold a free seat, check out,
// and sometimes cancel.
func buy(tok, eventID string) {
	var sm struct {
		Sections []struct {
			Seats []struct {
				ID    string `json:"event_seat_id"`
				State string `json:"state"`
			} `json:"seats"`
		} `json:"sections"`
	}
	status, err := request("GET", "/v1/events/"+eventID+"/seatmap", tok, nil, &sm)
	if !ok(status, err, "seatmap", 200) {
		return
	}
	var avail []string
	for _, s := range sm.Sections[0].Seats {
		if s.State == "available" {
			avail = append(avail, s.ID)
		}
	}
	if len(avail) == 0 {
		time.Sleep(50 * time.Millisecond) // sold out for now; cancellations will free some
		return
	}
	seat := avail[rand.IntN(len(avail))] //nolint:gosec // load pattern, not security

	var hold struct {
		ID string `json:"id"`
	}
	status, err = request("POST", "/v1/events/"+eventID+"/holds", tok, map[string]any{"seat_ids": []string{seat}}, &hold)
	switch {
	case status == 409:
		seatTaken.Add(1) // lost the race for that seat: normal contention
		return
	case !ok(status, err, "hold", 201):
		return
	}

	var order struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	status, err = request("POST", "/v1/holds/"+hold.ID+"/checkout", tok, nil, &order)
	if !ok(status, err, "checkout", 201, 202) {
		_, _ = request("DELETE", "/v1/holds/"+hold.ID, tok, nil, nil)
		return
	}
	if order.Status != "confirmed" {
		// 202: outcome unknown, the reconciler will settle it. Not a failure.
		return
	}
	confirmed.Add(1)
	if rand.Float64() < *cancelRate { //nolint:gosec // load pattern
		status, err = request("POST", "/v1/orders/"+order.ID+"/cancel", tok, nil, nil)
		if ok(status, err, "cancel", 200) {
			cancelled.Add(1)
		}
	}
}

// ok records anything other than an expected status as a failure. 429s are
// expected under load and counted separately.
func ok(status int, err error, op string, want ...int) bool {
	if err != nil {
		fail(op + ": " + err.Error())
		return false
	}
	for _, w := range want {
		if status == w {
			return true
		}
	}
	if status == 429 {
		rateLimited.Add(1)
		return false
	}
	fail(fmt.Sprintf("%s: HTTP %d", op, status))
	return false
}

// request sends one API call. Writes carry an Idempotency-Key that stays the same
// across retries; transport errors and 502/503/504 are retried with backoff.
func request(method, path, token string, body, out any) (int, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	key := uuid.NewString()
	var lastErr error
	for attempt := range 6 {
		if attempt > 0 {
			retries.Add(1)
			time.Sleep(time.Duration(50*(1<<attempt)) * time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, method, *base+path, bytes.NewReader(b))
		if err != nil {
			cancel()
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		switch resp.StatusCode {
		case 502, 503, 504:
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		if out != nil && resp.StatusCode < 300 {
			if err := json.Unmarshal(raw, out); err != nil {
				return resp.StatusCode, err
			}
		}
		return resp.StatusCode, nil
	}
	return 0, fmt.Errorf("gave up after retries: %w", lastErr)
}

func ensureLogin(email, password string) (string, error) {
	status, err := request("POST", "/v1/auth/register", "", map[string]string{"email": email, "password": password}, nil)
	if err != nil {
		return "", err
	}
	if status != 201 && status != 409 {
		return "", fmt.Errorf("register %s: HTTP %d", email, status)
	}
	var tp struct {
		AccessToken string `json:"access_token"`
	}
	status, err = request("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": password}, &tp)
	if err != nil {
		return "", fmt.Errorf("login %s: %w", email, err)
	}
	if status != 200 {
		return "", fmt.Errorf("login %s: HTTP %d", email, status)
	}
	return tp.AccessToken, nil
}

func setupEvent(admin string) (string, error) {
	rows := []map[string]any{}
	for left, r := *seats, 0; left > 0; r++ {
		n := min(left, 50)
		rows = append(rows, map[string]any{"label": fmt.Sprintf("R%02d", r), "seat_count": n})
		left -= n
	}
	var venue struct {
		ID       string `json:"id"`
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	if status, err := request("POST", "/v1/admin/venues", admin, map[string]any{
		"name": "Flash Sale Arena", "timezone": "UTC",
		"sections": []map[string]any{{"name": "Floor", "rows": rows}},
	}, &venue); err != nil || status != 201 {
		return "", httpErr("create venue", status, err)
	}
	var event struct {
		ID string `json:"id"`
	}
	if status, err := request("POST", "/v1/admin/events", admin, map[string]any{
		"venue_id": venue.ID, "name": "Flash Sale " + time.Now().Format(time.RFC3339),
		"starts_at":      time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"on_sale_at":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"section_prices": []map[string]any{{"section_id": venue.Sections[0].ID, "price_cents": 7500}},
	}, &event); err != nil || status != 201 {
		return "", httpErr("create event", status, err)
	}
	if status, err := request("POST", "/v1/admin/events/"+event.ID+"/publish", admin, nil, nil); err != nil || status != 200 {
		return "", httpErr("publish", status, err)
	}
	return event.ID, nil
}

func report(inv map[string]int) {
	keys := make([]string, 0, len(failures))
	for k := range failures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("confirmed=%d cancelled=%d seat_taken=%d rate_limited=%d retries=%d failed=%d\n",
		confirmed.Load(), cancelled.Load(), seatTaken.Load(), rateLimited.Load(), retries.Load(), failed.Load())
	for _, k := range keys {
		fmt.Printf("  failure: %s x%d\n", k, failures[k])
	}
	fmt.Printf("invariants: %v\n", inv)
}

func parallel(n, workers int, fn func(i int) error) error {
	jobs := make(chan int)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(i); err != nil {
					errs <- err
				}
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	return <-errs
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func httpErr(op string, status int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: HTTP %d", op, status)
}
