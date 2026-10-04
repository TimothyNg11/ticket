// Command onsale simulates a high-demand on-sale end to end, the scenario in
// the spec: N buyers (50,000 by default) rush the waiting room of an event with
// a few thousand seats. Waiting buyers poll their place in line and the seat map;
// admitted buyers pick seats, hold them, and check out; everyone stops once the
// event sells out. Afterwards it prints latency per operation and checks the
// system's invariants.
//
// Each buyer is a goroutine, so 50,000 concurrent buyers cost a few hundred MB,
// which is why this is Go rather than k6 (one JavaScript VM per virtual user).
//
//	go run ./loadtest/onsale -base http://localhost -tokens loadtest-tokens.txt \
//	    -admin-token-file loadtest-admin-token.txt
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ticket/internal/loadclient"
)

var (
	base      = flag.String("base", "http://localhost", "API base URL")
	tokens    = flag.String("tokens", "loadtest-tokens.txt", "buyer access tokens, one per line")
	adminFile = flag.String("admin-token-file", "loadtest-admin-token.txt", "admin access token")
	users     = flag.Int("users", 50000, "buyers to simulate (<= tokens in the file)")
	seats     = flag.Int("seats", 5000, "seats in the event")
	batch     = flag.Int("batch", 500, "waiting-room admission batch")
	interval  = flag.Int("interval", 10, "seconds between admission batches")
	joinOver  = flag.Duration("join-over", 30*time.Second, "spread the initial rush over this long")
	poll      = flag.Duration("poll", 0, "fixed poll interval; 0 follows the server's poll_after_seconds")
	maxConns  = flag.Int("conns", 2048, "client connections shared by all buyers")
	maxTime   = flag.Duration("max", 20*time.Minute, "give up after this long")
	insecure  = flag.Bool("insecure", false, "skip TLS verification (local self-signed)")
	label     = flag.String("label", "on-sale", "name for this run in the report")
)

var (
	admitted, soldOutSeen, bought, seatsBought, holdLost, gaveUp atomic.Int64
	firstSoldOut                                                 atomic.Int64 // unix ms
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ONSALE FAIL:", err)
		os.Exit(1)
	}
}

func run() error {
	toks, err := readLines(*tokens)
	if err != nil {
		return err
	}
	if len(toks) < *users {
		return fmt.Errorf("need %d tokens, file has %d", *users, len(toks))
	}
	adminTok, err := readLines(*adminFile)
	if err != nil {
		return err
	}
	c := loadclient.New(*base, *maxConns, *insecure)
	ctx, cancel := context.WithTimeout(context.Background(), *maxTime)
	defer cancel()

	eventID, err := createEvent(ctx, c, adminTok[0])
	if err != nil {
		return err
	}
	fmt.Printf("[%s] event %s: %d seats, %d buyers, admit %d every %ds\n", *label, eventID, *seats, *users, *batch, *interval)
	c.Rec = loadclient.NewRecorder() // measure the sale only, not setup

	start := time.Now()
	var wg sync.WaitGroup
	for i := range *users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buyer(ctx, c, toks[i], eventID)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-tick.C:
			fmt.Printf("  t=%3.0fs admitted=%d bought=%d seats=%d lost_races=%d\n",
				time.Since(start).Seconds(), admitted.Load(), bought.Load(), seatsBought.Load(), holdLost.Load())
		}
	}
	elapsed := time.Since(start)

	fmt.Printf("\n[%s] %d buyers in %s; %d admitted, %d orders, %d seats sold, %d lost a seat race, %d gave up\n",
		*label, *users, elapsed.Round(time.Second), admitted.Load(), bought.Load(), seatsBought.Load(), holdLost.Load(), gaveUp.Load())
	if ms := firstSoldOut.Load(); ms > 0 {
		fmt.Printf("sold out after %s\n", time.UnixMilli(ms).Sub(start).Round(time.Second))
	}
	c.Rec.Print(os.Stdout)

	r, err := c.Do(context.Background(), "invariants", "GET", "/v1/admin/invariants", adminTok[0], nil, nil)
	if err != nil {
		return err
	}
	fmt.Printf("invariants: %s\n", strings.TrimSpace(string(r.Body)))
	var inv struct {
		Total int `json:"total"`
	}
	if err := r.JSON(&inv); err != nil || inv.Total != 0 {
		return fmt.Errorf("invariant violations: %s", r.Body)
	}
	ops, _, _ := c.Rec.Report()
	failed := 0
	for _, o := range ops {
		failed += o.Failed
	}
	if failed > 0 {
		return fmt.Errorf("%d requests failed after retries", failed)
	}
	fmt.Println("ONSALE OK")
	return nil
}

type queueStatus struct {
	QueueToken     string `json:"queue_token"`
	Admitted       bool   `json:"admitted"`
	SoldOut        bool   `json:"sold_out"`
	AdmissionToken string `json:"admission_token"`
	PollAfter      int    `json:"poll_after_seconds"`
}

// buyer is one person: join the queue, wait, buy if admitted, leave if sold out.
func buyer(ctx context.Context, c *loadclient.Client, tok, eventID string) {
	sleep(ctx, time.Duration(rand.Int64N(int64(*joinOver)+1))) //nolint:gosec // load pattern
	var st queueStatus
	if !getJSON(ctx, c, "queue_join", "POST", "/v1/events/"+eventID+"/queue", tok, nil, nil, &st) {
		gaveUp.Add(1)
		return
	}
	for ctx.Err() == nil {
		if st.SoldOut {
			soldOutSeen.Add(1)
			firstSoldOut.CompareAndSwap(0, time.Now().UnixMilli())
			return
		}
		if st.Admitted {
			admitted.Add(1)
			buy(ctx, c, tok, eventID, st.AdmissionToken)
			return
		}
		// While waiting, people refresh their place in line and the seat map, as
		// often as the server asks (real queue pages poll on the server's cue).
		wait := *poll
		if wait == 0 {
			wait = time.Duration(max(st.PollAfter, 1)) * time.Second
		}
		sleep(ctx, jitter(wait))
		getJSON(ctx, c, "seatmap", "GET", "/v1/events/"+eventID+"/seatmap", tok, nil, nil, nil)
		if !getJSON(ctx, c, "queue_poll", "GET", "/v1/queue/"+st.QueueToken, tok, nil, nil, &st) {
			gaveUp.Add(1)
			return
		}
	}
}

// buy picks 1-4 available seats (most people buy 2), holds them, and checks out.
// Losing a seat race (409) means someone else got there first: look again.
func buy(ctx context.Context, c *loadclient.Client, tok, eventID, admission string) {
	want := []int{1, 2, 2, 2, 3, 4}[rand.IntN(6)] //nolint:gosec // load pattern
	adm := map[string]string{"Admission-Token": admission}
	for range 5 {
		var sm struct {
			Sections []struct {
				Seats []struct {
					ID    string `json:"event_seat_id"`
					State string `json:"state"`
				} `json:"seats"`
			} `json:"sections"`
		}
		if !getJSON(ctx, c, "seatmap", "GET", "/v1/events/"+eventID+"/seatmap", tok, nil, nil, &sm) {
			return
		}
		var free []string
		for _, sec := range sm.Sections {
			for _, s := range sec.Seats {
				if s.State == "available" {
					free = append(free, s.ID)
				}
			}
		}
		if len(free) == 0 {
			firstSoldOut.CompareAndSwap(0, time.Now().UnixMilli())
			return
		}
		rand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] }) //nolint:gosec // load pattern
		pick := free[:min(want, len(free))]

		r, err := c.Do(ctx, "hold", "POST", "/v1/events/"+eventID+"/holds", tok, map[string]any{"seat_ids": pick}, adm)
		if err != nil {
			return
		}
		if r.Status == 409 {
			holdLost.Add(1)
			continue
		}
		if r.Status != 201 {
			return
		}
		var hold struct {
			ID string `json:"id"`
		}
		_ = r.JSON(&hold)
		r, err = c.Do(ctx, "checkout", "POST", "/v1/holds/"+hold.ID+"/checkout", tok, nil, nil)
		if err == nil && (r.Status == 201 || r.Status == 202) {
			bought.Add(1)
			seatsBought.Add(int64(len(pick)))
			return
		}
		// Giving up: let the seats go now rather than in 10 minutes.
		_, _ = c.Do(ctx, "release", "DELETE", "/v1/holds/"+hold.ID, tok, nil, nil)
		return
	}
}

func getJSON(ctx context.Context, c *loadclient.Client, op, method, path, tok string, body any, extra map[string]string, out any) bool {
	r, err := c.Do(ctx, op, method, path, tok, body, extra)
	if err != nil || r.Status >= 300 {
		return false
	}
	if out != nil {
		return r.JSON(out) == nil
	}
	return true
}

func createEvent(ctx context.Context, c *loadclient.Client, admin string) (string, error) {
	var rows []map[string]any
	for left, r := *seats, 0; left > 0; r++ {
		n := min(left, 50)
		rows = append(rows, map[string]any{"label": fmt.Sprintf("R%03d", r), "seat_count": n})
		left -= n
	}
	var venue struct {
		ID       string `json:"id"`
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	if !getJSON(ctx, c, "setup", "POST", "/v1/admin/venues", admin, map[string]any{
		"name": "Load Test Stadium", "timezone": "UTC",
		"sections": []map[string]any{{"name": "Floor", "rows": rows}},
	}, nil, &venue) {
		return "", errors.New("create venue failed")
	}
	var ev struct {
		ID string `json:"id"`
	}
	if !getJSON(ctx, c, "setup", "POST", "/v1/admin/events", admin, map[string]any{
		"venue_id": venue.ID, "name": "Load Test " + time.Now().Format(time.RFC3339),
		"starts_at":      time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339),
		"on_sale_at":     time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"queue":          map[string]any{"enabled": true, "batch_size": *batch, "interval_seconds": *interval},
		"section_prices": []map[string]any{{"section_id": venue.Sections[0].ID, "price_cents": 12500}},
	}, nil, &ev) {
		return "", errors.New("create event failed")
	}
	if !getJSON(ctx, c, "setup", "POST", "/v1/admin/events/"+ev.ID+"/publish", admin, nil, nil, nil) {
		return "", errors.New("publish failed")
	}
	return ev.ID, nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d)+1)) //nolint:gosec // load pattern
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
