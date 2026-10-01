// Command smoke checks the Phase 1 "done when" against a running stack: an admin
// sets up a venue and published event, then a fresh user registers, logs in, and
// fetches the event's seat map. Exit status 0 means it all worked.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

var base = envOr("BASE_URL", "http://localhost:8080")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "SMOKE FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("SMOKE OK")
}

func run() error {
	adminEmail := envOr("ADMIN_EMAIL", "admin@example.com")
	adminPW := envOr("ADMIN_PASSWORD", "smoke-admin-password")

	// The admin may already exist from an earlier run; 409 is fine.
	if code, body, err := do("POST", "/v1/auth/register", "", map[string]string{"email": adminEmail, "password": adminPW}); err != nil {
		return err
	} else if code != 201 && code != 409 {
		return fmt.Errorf("register admin: %d %s", code, body)
	}
	admin, err := login(adminEmail, adminPW)
	if err != nil {
		return err
	}

	var venue struct {
		ID       string `json:"id"`
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	if err := expect(201, &venue, "POST", "/v1/admin/venues", admin, map[string]any{
		"name": "Smoke Arena", "timezone": "UTC",
		"sections": []map[string]any{{"name": "Floor", "rows": []map[string]any{{"label": "A", "seat_count": 10}}}},
	}); err != nil {
		return fmt.Errorf("create venue: %w", err)
	}
	var event struct {
		ID string `json:"id"`
	}
	if err := expect(201, &event, "POST", "/v1/admin/events", admin, map[string]any{
		"venue_id": venue.ID, "name": "Smoke Show",
		"starts_at":      time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"on_sale_at":     time.Now().UTC().Format(time.RFC3339),
		"section_prices": []map[string]any{{"section_id": venue.Sections[0].ID, "price_cents": 2500}},
	}); err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	if err := expect(200, nil, "POST", "/v1/admin/events/"+event.ID+"/publish", admin, nil); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	// The actual "done when": a brand-new user registers, logs in, and reads the seat map.
	email := "smoke-" + uuid.NewString()[:8] + "@example.com"
	if err := expect(201, nil, "POST", "/v1/auth/register", "", map[string]string{"email": email, "password": "smoke-user-password"}); err != nil {
		return fmt.Errorf("register user: %w", err)
	}
	user, err := login(email, "smoke-user-password")
	if err != nil {
		return err
	}
	var sm struct {
		Sections []struct {
			Seats []struct {
				State string `json:"state"`
			} `json:"seats"`
		} `json:"sections"`
	}
	if err := expect(200, &sm, "GET", "/v1/events/"+event.ID+"/seatmap", user, nil); err != nil {
		return fmt.Errorf("seat map: %w", err)
	}
	if len(sm.Sections) != 1 || len(sm.Sections[0].Seats) != 10 {
		return fmt.Errorf("seat map: want 1 section with 10 seats, got %+v", sm)
	}
	return nil
}

func login(email, pw string) (string, error) {
	var tp struct {
		AccessToken string `json:"access_token"`
	}
	if err := expect(200, &tp, "POST", "/v1/auth/login", "", map[string]string{"email": email, "password": pw}); err != nil {
		return "", fmt.Errorf("login %s: %w", email, err)
	}
	return tp.AccessToken, nil
}

func expect(want int, out any, method, path, token string, body any) error {
	code, raw, err := do(method, path, token, body)
	if err != nil {
		return err
	}
	if code != want {
		return fmt.Errorf("%s %s: want %d, got %d: %s", method, path, want, code, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func do(method, path, token string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
