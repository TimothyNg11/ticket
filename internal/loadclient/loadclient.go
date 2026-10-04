// Package loadclient is the HTTP client the load and chaos tools use. It behaves
// like a well-written real client: writes carry an Idempotency-Key that stays the
// same across retries, and transport errors and 502/503/504 are retried with
// backoff. It also records per-operation latency for reporting.
package loadclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Client talks to one API base URL.
type Client struct {
	Base string
	HC   *http.Client
	Rec  *Recorder
	// Header, if set, adds headers to every request (e.g. Admission-Token).
	Retries int
}

// New returns a client sharing at most maxConns connections. Thousands of
// simulated users share them, the way a fleet of browsers shares a load balancer.
func New(base string, maxConns int, insecure bool) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxConnsPerHost = maxConns
	t.MaxIdleConnsPerHost = maxConns
	t.MaxIdleConns = maxConns
	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, local self-signed TLS only
	}
	// Ask for gzip ourselves and keep bodies compressed: thousands of simulated
	// users poll big seat maps whose contents they mostly ignore, and
	// decompressing every one would make the load generator, not the server,
	// the bottleneck. Response.JSON decompresses on demand.
	t.DisableCompression = true
	return &Client{Base: base, HC: &http.Client{Transport: t, Timeout: 30 * time.Second}, Rec: NewRecorder(), Retries: 5}
}

// Response is the result of one logical call (after retries).
type Response struct {
	Status int
	Body   []byte // as received: possibly gzip-compressed (see Gzipped)
	// Gzipped reports whether Body is gzip-compressed.
	Gzipped bool
}

// JSON decodes the body into out, decompressing it first if needed.
func (r Response) JSON(out any) error {
	var rd io.Reader = bytes.NewReader(r.Body)
	if r.Gzipped {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return err
		}
		rd = zr
	}
	return json.NewDecoder(rd).Decode(out)
}

// Do sends one call. op names it in the latency report. extra headers are
// optional. Writes get an Idempotency-Key reused across retries.
func (c *Client) Do(ctx context.Context, op, method, path, token string, body any, extra map[string]string) (Response, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return Response{}, err
		}
	}
	key := uuid.NewString()
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			c.Rec.retry(op)
			time.Sleep(time.Duration(50*(1<<min(attempt, 5))) * time.Millisecond)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(b))
		if err != nil {
			return Response{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		if method != http.MethodGet {
			req.Header.Set("Idempotency-Key", key)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		// Time from getting a connection to the full response: the server's
		// latency, excluding time this process spent waiting for a free socket.
		start := time.Now()
		var gotConn time.Time
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { gotConn = time.Now() },
		}))
		resp, err := c.HC.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		end := time.Now()
		if gotConn.IsZero() {
			gotConn = start
		}
		c.Rec.observe(op, resp.StatusCode, end.Sub(gotConn), gotConn.Sub(start))
		if rerr != nil {
			lastErr = rerr
			continue
		}
		switch resp.StatusCode {
		case 502, 503, 504:
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		return Response{Status: resp.StatusCode, Body: raw, Gzipped: resp.Header.Get("Content-Encoding") == "gzip"}, nil
	}
	c.Rec.failure(op)
	return Response{}, fmt.Errorf("%s: gave up after retries: %w", op, lastErr)
}

// Recorder collects latencies and status counts per operation.
type Recorder struct {
	mu   sync.Mutex
	ops  map[string]*opStats
	from time.Time
}

type opStats struct {
	lat      []time.Duration
	connWait []time.Duration
	status   map[int]int
	retries  int
	failures int
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{ops: map[string]*opStats{}, from: time.Now()} }

func (r *Recorder) get(op string) *opStats {
	s, ok := r.ops[op]
	if !ok {
		s = &opStats{status: map[int]int{}}
		r.ops[op] = s
	}
	return s
}

func (r *Recorder) observe(op string, status int, lat, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(op)
	s.lat = append(s.lat, lat)
	s.connWait = append(s.connWait, wait)
	s.status[status]++
}

func (r *Recorder) retry(op string) {
	r.mu.Lock()
	r.get(op).retries++
	r.mu.Unlock()
}

func (r *Recorder) failure(op string) {
	r.mu.Lock()
	r.get(op).failures++
	r.mu.Unlock()
}

// OpReport summarizes one operation.
type OpReport struct {
	Op                         string
	Count                      int
	P50, P95, P99, Max         time.Duration
	ConnWaitP99                time.Duration
	Status                     map[int]int
	Server5xx, Retries, Failed int
}

// Report summarizes every operation, sorted by name.
func (r *Recorder) Report() (ops []OpReport, total int, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, s := range r.ops {
		rep := OpReport{Op: name, Count: len(s.lat), Status: s.status, Retries: s.retries, Failed: s.failures}
		if len(s.lat) > 0 {
			l := append([]time.Duration(nil), s.lat...)
			sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
			q := func(p float64) time.Duration { return l[min(len(l)-1, int(p*float64(len(l))))] }
			rep.P50, rep.P95, rep.P99, rep.Max = q(.50), q(.95), q(.99), l[len(l)-1]
			w := append([]time.Duration(nil), s.connWait...)
			sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
			rep.ConnWaitP99 = w[min(len(w)-1, int(.99*float64(len(w))))]
		}
		for code, n := range s.status {
			if code >= 500 {
				rep.Server5xx += n
			}
		}
		total += rep.Count
		ops = append(ops, rep)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Op < ops[j].Op })
	return ops, total, time.Since(r.from)
}

// Print writes a human-readable table of the report.
func (r *Recorder) Print(w io.Writer) {
	ops, total, elapsed := r.Report()
	_, _ = fmt.Fprintf(w, "%-14s %8s %8s %8s %8s %8s %10s %6s %6s %6s\n", "op", "count", "p50", "p95", "p99", "max", "connwait99", "5xx", "retry", "FAIL")
	for _, o := range ops {
		_, _ = fmt.Fprintf(w, "%-14s %8d %8s %8s %8s %8s %10s %6d %6d %6d\n", o.Op, o.Count,
			ms(o.P50), ms(o.P95), ms(o.P99), ms(o.Max), ms(o.ConnWaitP99), o.Server5xx, o.Retries, o.Failed)
	}
	_, _ = fmt.Fprintf(w, "total %d requests in %s = %.0f req/s\n", total, elapsed.Round(time.Second), float64(total)/elapsed.Seconds())
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0fms", float64(d.Microseconds())/1000) }
