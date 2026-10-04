package errortrack

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sendPlugin is a test plugin sending to fake, logging to log, on clock.
func sendPlugin(t *testing.T, fake *fakeSentry, o Options, clock *fakeClock, log *syncBuffer) *Plugin {
	t.Helper()
	p := testPlugin(o)
	p.dsn = fake.dsn(t)
	if clock != nil {
		p.now = clock.now
	}
	if log != nil {
		p.log = slog.New(slog.NewTextHandler(log, nil))
	}
	return p
}

// fullEvent sets every field an event has.
func fullEvent() *Event {
	return &Event{
		EventID:   "0123456789abcdef0123456789abcdef",
		Timestamp: time.Date(2026, 10, 4, 12, 0, 0, 500_000_000, time.UTC),
		Level:     "fatal",
		Exceptions: []Exception{
			{Type: "*errors.errorString", Value: "inner"},
			{Type: "*collage.PanicError", Value: "boom", Frames: []Frame{
				{Function: "main.handler", File: "/app/main.go", Line: 12, InApp: true},
				{Function: "net/http.serve", File: "/go/src/net/http/server.go", Line: 99},
			}},
		},
		Tags: map[string]string{"stage": "page", "status": "500"},
		Request: &RequestInfo{
			Method: "GET", URL: "https://example.com/post/{slug}", Query: "q=%5Bfiltered%5D",
			Headers: map[string]string{"User-Agent": "test"}, IP: "203.0.113.9",
		},
		User:        &User{ID: "7", Username: "ada", Email: "ada@example.com"},
		Environment: "production",
		Release:     "build-1",
		ServerName:  "host-1",
		Transaction: "/post/{slug}",
	}
}

// Typed mirrors of the wire format, with the keys Sentry reads.
type (
	wantHeader struct {
		EventID string `json:"event_id"`
		SentAt  string `json:"sent_at"`
	}
	wantItem struct {
		Type   string `json:"type"`
		Length int    `json:"length"`
	}
	wantEvent struct {
		EventID     string            `json:"event_id"`
		Timestamp   string            `json:"timestamp"`
		Platform    string            `json:"platform"`
		Level       string            `json:"level"`
		Environment string            `json:"environment"`
		Release     string            `json:"release"`
		ServerName  string            `json:"server_name"`
		Transaction string            `json:"transaction"`
		Tags        map[string]string `json:"tags"`
		User        *struct {
			ID        string `json:"id"`
			Username  string `json:"username"`
			Email     string `json:"email"`
			IPAddress string `json:"ip_address"`
		} `json:"user"`
		Request *struct {
			Method      string            `json:"method"`
			URL         string            `json:"url"`
			QueryString string            `json:"query_string"`
			Headers     map[string]string `json:"headers"`
		} `json:"request"`
		Exception *struct {
			Values []struct {
				Type       string `json:"type"`
				Value      string `json:"value"`
				Stacktrace *struct {
					Frames []struct {
						Function string `json:"function"`
						Filename string `json:"filename"`
						Lineno   int    `json:"lineno"`
						InApp    bool   `json:"in_app"`
					} `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		} `json:"exception"`
		SDK struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"sdk"`
	}
)

// splitEnvelope checks the envelope's three lines and decodes the event.
func splitEnvelope(t *testing.T, body []byte) (wantHeader, wantEvent, string) {
	t.Helper()
	lines := strings.Split(string(body), "\n")
	if len(lines) != 3 {
		t.Fatalf("envelope has %d lines, want 3:\n%s", len(lines), body)
	}
	var h wantHeader
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatalf("header: %v", err)
	}
	var item wantItem
	if err := json.Unmarshal([]byte(lines[1]), &item); err != nil {
		t.Fatalf("item header: %v", err)
	}
	if want := `{"type":"event","length":` + strconv.Itoa(len(lines[2])) + `}`; lines[1] != want {
		t.Errorf("item header = %s, want %s", lines[1], want)
	}
	var e wantEvent
	if err := json.Unmarshal([]byte(lines[2]), &e); err != nil {
		t.Fatalf("event: %v", err)
	}
	return h, e, lines[2]
}

func TestEnvelope(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 1, 0, time.UTC)
	body, err := envelope(fullEvent(), now)
	if err != nil {
		t.Fatal(err)
	}
	h, e, raw := splitEnvelope(t, body)
	if h.EventID != "0123456789abcdef0123456789abcdef" || h.SentAt != "2026-10-04T12:00:01Z" {
		t.Errorf("header = %+v", h)
	}
	if e.EventID != h.EventID || e.Timestamp != "2026-10-04T12:00:00.5Z" || e.Platform != "go" ||
		e.Level != "fatal" || e.Environment != "production" || e.Release != "build-1" ||
		e.ServerName != "host-1" || e.Transaction != "/post/{slug}" {
		t.Errorf("event = %+v", e)
	}
	if e.Tags["stage"] != "page" || e.Tags["status"] != "500" {
		t.Errorf("tags = %v", e.Tags)
	}
	if u := e.User; u == nil || u.ID != "7" || u.Username != "ada" || u.Email != "ada@example.com" || u.IPAddress != "203.0.113.9" {
		t.Errorf("user = %+v", e.User)
	}
	if r := e.Request; r == nil || r.Method != "GET" || r.URL != "https://example.com/post/{slug}" ||
		r.QueryString != "q=%5Bfiltered%5D" || r.Headers["User-Agent"] != "test" {
		t.Errorf("request = %+v", e.Request)
	}
	if e.Exception == nil || len(e.Exception.Values) != 2 {
		t.Fatalf("exception = %+v", e.Exception)
	}
	inner, outer := e.Exception.Values[0], e.Exception.Values[1]
	if inner.Type != "*errors.errorString" || inner.Value != "inner" || inner.Stacktrace != nil {
		t.Errorf("inner = %+v", inner)
	}
	if outer.Type != "*collage.PanicError" || outer.Value != "boom" || outer.Stacktrace == nil || len(outer.Stacktrace.Frames) != 2 {
		t.Fatalf("outer = %+v", outer)
	}
	f0, f1 := outer.Stacktrace.Frames[0], outer.Stacktrace.Frames[1]
	if f0.Function != "main.handler" || f0.Filename != "/app/main.go" || f0.Lineno != 12 || !f0.InApp {
		t.Errorf("frame 0 = %+v", f0)
	}
	if f1.Function != "net/http.serve" || f1.InApp || !strings.Contains(raw, `"in_app":false`) {
		t.Errorf("frame 1 = %+v", f1)
	}
	if e.SDK.Name != "collage-errortrack" || e.SDK.Version != (&Plugin{}).Version() {
		t.Errorf("sdk = %+v", e.SDK)
	}
}

func TestEnvelope_OmitsEmpty(t *testing.T) {
	body, err := envelope(&Event{EventID: "0123456789abcdef0123456789abcdef", Level: "error"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, _, raw := splitEnvelope(t, body)
	for _, key := range []string{"null", `"user"`, `"request"`, `"tags"`, `"exception"`, `"release"`, `"server_name"`} {
		if strings.Contains(raw, key) {
			t.Errorf("an empty event carries %s: %s", key, raw)
		}
	}
	// The IP alone still makes a user, as Sentry reads it there.
	body, _ = envelope(&Event{EventID: "0123456789abcdef0123456789abcdef", Request: &RequestInfo{IP: "203.0.113.9"}}, time.Now())
	if _, e, _ := splitEnvelope(t, body); e.User == nil || e.User.IPAddress != "203.0.113.9" || e.User.ID != "" {
		t.Errorf("user = %+v", e.User)
	}
}

func TestSend_Posts(t *testing.T) {
	fake := newFakeSentry(t)
	p := sendPlugin(t, fake, Options{}, nil, nil)
	p.startSender()
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	p.enqueue(fullEvent())
	waitFor(t, func() bool { return fake.count() == 1 })

	r := fake.received()[0]
	if r.Method != http.MethodPost || r.Path != "/api/123/envelope/" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	if r.Auth != p.dsn.auth(p.Version()) || !strings.Contains(r.Auth, "sentry_key=fakekey") {
		t.Errorf("X-Sentry-Auth = %q", r.Auth)
	}
	if r.ContentType != "application/x-sentry-envelope" {
		t.Errorf("Content-Type = %q", r.ContentType)
	}
	if h, _, _ := splitEnvelope(t, r.Body); h.EventID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("header = %+v", h)
	}
}

func TestSend_NeverBlocks(t *testing.T) {
	fake := newFakeSentry(t)
	fake.set(func(f *fakeSentry) { f.hanging = true })
	const size = 10
	p := sendPlugin(t, fake, Options{QueueSize: size}, nil, nil)
	p.startSender()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	start := time.Now()
	for range 1000 {
		p.enqueue(fullEvent())
		if n := len(p.queue); n > size {
			t.Fatalf("queue length %d exceeds %d", n, size)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("1000 enqueues took %v", d)
	}
	if n := p.dropped.Load(); n < 1000-size-1 {
		t.Errorf("dropped = %d, want at least %d", n, 1000-size-1)
	}
}

func TestSend_PerMinute(t *testing.T) {
	fake := newFakeSentry(t)
	clock := newFakeClock()
	p := sendPlugin(t, fake, Options{PerMinute: 3}, clock, nil)
	for range 5 {
		p.deliver(fullEvent())
	}
	if n := fake.count(); n != 3 {
		t.Fatalf("sent %d of 5, want 3", n)
	}
	if n := p.dropped.Load(); n != 2 {
		t.Errorf("dropped = %d, want 2", n)
	}
	clock.advance(61 * time.Second)
	p.deliver(fullEvent())
	if n := fake.count(); n != 4 {
		t.Errorf("after a minute sent %d, want 4", n)
	}
}

func TestSend_RateLimited429(t *testing.T) {
	tests := map[string]struct {
		retryAfter, rateLimits string
		pause                  time.Duration
	}{
		"retry-after seconds": {retryAfter: "30", pause: 30 * time.Second},
		"retry-after date":    {retryAfter: "Sun, 04 Oct 2026 12:00:45 GMT", pause: 45 * time.Second},
		"rate limits":         {rateLimits: "20:error:org, 40::key", retryAfter: "5", pause: 40 * time.Second},
		"neither":             {pause: 60 * time.Second},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fake := newFakeSentry(t)
			fake.set(func(f *fakeSentry) {
				f.status, f.retryAfter, f.rateLimits = http.StatusTooManyRequests, tc.retryAfter, tc.rateLimits
			})
			clock := newFakeClock()
			p := sendPlugin(t, fake, Options{}, clock, &syncBuffer{})
			p.deliver(fullEvent())
			fake.set(func(f *fakeSentry) { f.status, f.retryAfter, f.rateLimits = http.StatusOK, "", "" })

			clock.advance(tc.pause - time.Second)
			before := p.dropped.Load()
			p.deliver(fullEvent())
			if n := fake.count(); n != 1 {
				t.Errorf("sent during the pause: %d requests", n)
			}
			if p.dropped.Load() != before+1 {
				t.Error("the paused event was not counted")
			}
			clock.advance(time.Second)
			p.deliver(fullEvent())
			if n := fake.count(); n != 2 {
				t.Errorf("after the pause %d requests, want 2", n)
			}
		})
	}
}

func TestSend_ErrorLogsNoKey(t *testing.T) {
	fake := newFakeSentry(t)
	fake.set(func(f *fakeSentry) { f.status = http.StatusInternalServerError })
	clock := newFakeClock()
	log := &syncBuffer{}
	p := sendPlugin(t, fake, Options{}, clock, log)
	full := strings.Replace(fake.srv.URL, "://", "://fakekey@", 1) + "/123"

	p.deliver(fullEvent())
	if !strings.Contains(log.String(), "500") {
		t.Errorf("the status is not logged: %s", log)
	}

	// A transport error: nothing listens at a closed server's address.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	down := sendPlugin(t, fake, Options{}, clock, log)
	down.dsn.Host = strings.TrimPrefix(closed.URL, "http://")
	down.deliver(fullEvent())

	out := log.String()
	if strings.Count(out, "level=ERROR") < 2 {
		t.Errorf("want two failures logged:\n%s", out)
	}
	if !strings.Contains(out, fake.dsn(t).Host) || !strings.Contains(out, down.dsn.Host) {
		t.Errorf("the host is not logged:\n%s", out)
	}
	for _, secret := range []string{"fakekey", full, fake.dsn(t).endpoint(), down.dsn.endpoint()} {
		if strings.Contains(out, secret) {
			t.Errorf("the log carries %q:\n%s", secret, out)
		}
	}

	// Drops are logged at most once a minute.
	for range 5 {
		p.dropped.Add(1)
		p.reportDrops()
	}
	if n := strings.Count(log.String(), "dropped"); n != 1 {
		t.Errorf("drops logged %d times in a minute, want 1:\n%s", n, log)
	}
	clock.advance(time.Minute)
	p.reportDrops()
	if n := strings.Count(log.String(), "dropped"); n != 2 {
		t.Errorf("drops logged %d times after a minute, want 2:\n%s", n, log)
	}
}

func TestShutdown_Drains(t *testing.T) {
	fake := newFakeSentry(t)
	p := sendPlugin(t, fake, Options{}, nil, nil)
	p.startSender()
	for range 5 {
		p.enqueue(fullEvent())
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.count(); n != 5 {
		t.Errorf("%d events reached the fake before Shutdown returned, want 5", n)
	}
	p.enqueue(fullEvent()) // after Shutdown: no panic
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("a second Shutdown: %v", err)
	}
}

func TestShutdown_RespectsContext(t *testing.T) {
	fake := newFakeSentry(t)
	fake.set(func(f *fakeSentry) { f.hanging = true })
	p := sendPlugin(t, fake, Options{Timeout: Duration(time.Hour)}, nil, nil)
	p.startSender()
	for range 5 {
		p.enqueue(fullEvent())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.Shutdown(ctx)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("Shutdown took %v", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown = %v, want the context's error", err)
	}
	// The hanging send is cancelled, and the sender ends without sending the rest.
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Error("the sender still hangs after Shutdown's context ended")
	}
	if n := p.dropped.Load(); n != 5 {
		t.Errorf("dropped = %d, want the 5 left", n)
	}
}

func TestShutdown_NotStarted(t *testing.T) {
	p := testPlugin(Options{})
	p.enqueue(fullEvent()) // disabled: no queue, no panic
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
