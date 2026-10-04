package errortrack

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSentry is an httptest server recording the envelopes POSTed to it.
type fakeSentry struct {
	srv  *httptest.Server
	hang chan struct{} // closed at cleanup; until then a hanging fake blocks

	mu         sync.Mutex
	status     int    // answer, default 200
	retryAfter string // Retry-After on the answer, when set
	rateLimits string // X-Sentry-Rate-Limits on the answer, when set
	hanging    bool   // block every request until cleanup or its context ends
	requests   []fakeRequest
}

// fakeRequest is one request the fake received.
type fakeRequest struct {
	Method, Path, Auth, ContentType string
	Body                            []byte
}

// newFakeSentry starts a fake; its DSN is fake.dsn().
func newFakeSentry(t *testing.T) *fakeSentry {
	t.Helper()
	f := &fakeSentry{hang: make(chan struct{}), status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	// Cleanups run last first: hang is closed before Close waits for handlers.
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.hang) })
	return f
}

func (f *fakeSentry) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	hanging := f.hanging
	status, retryAfter, rateLimits := f.status, f.retryAfter, f.rateLimits
	f.mu.Unlock()
	if hanging {
		select {
		case <-f.hang:
		case <-r.Context().Done():
		}
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{
		Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("X-Sentry-Auth"),
		ContentType: r.Header.Get("Content-Type"), Body: body,
	})
	f.mu.Unlock()
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	if rateLimits != "" {
		w.Header().Set("X-Sentry-Rate-Limits", rateLimits)
	}
	w.WriteHeader(status)
}

// dsn is a DSN pointing at the fake, with key "fakekey" and project 123.
func (f *fakeSentry) dsn(t *testing.T) dsn {
	t.Helper()
	d, err := parseDSN(strings.Replace(f.srv.URL, "://", "://fakekey@", 1) + "/123")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fakeSentry) set(fn func(f *fakeSentry)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeSentry) received() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeSentry) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// waitFor polls until cond holds or a second passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// syncBuffer is a log destination safe to write and read from two goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
