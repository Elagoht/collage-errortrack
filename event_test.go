package errortrack

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/Elagoht/collage/pkg/collage"
)

// panicStack panics with err and returns the stack recovered from it.
func panicStack(err error) (stack []byte) {
	defer func() {
		_ = recover()
		stack = debug.Stack()
	}()
	panic(err)
}

// panicErr is what the core reports for a panic with err as its value.
func panicErr(err error) error {
	return fmt.Errorf("%w: %w", collage.ErrPanic, &collage.PanicError{Value: err, Stack: panicStack(err)})
}

// cyclic is an error whose Unwrap returns itself.
type cyclic struct{}

func (c *cyclic) Error() string { return "cyclic" }
func (c *cyclic) Unwrap() error { return c }

// cyclicJoin is a multi-error that contains itself.
type cyclicJoin struct{}

func (c *cyclicJoin) Error() string   { return "cyclic join" }
func (c *cyclicJoin) Unwrap() []error { return []error{c} }

func TestExceptions_Chain(t *testing.T) {
	err := fmt.Errorf("handler: %w", fmt.Errorf("db: %w", io.EOF))
	got := exceptions(err)
	want := []Exception{
		{Type: "*errors.errorString", Value: "EOF"},
		{Type: "*fmt.wrapError", Value: "db: EOF"},
		{Type: "*fmt.wrapError", Value: "handler: db: EOF"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Type != want[i].Type || got[i].Value != want[i].Value || got[i].Frames != nil {
			t.Errorf("exception %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestExceptions_PanicWithError(t *testing.T) {
	inner := errors.New("db down")
	err := panicErr(inner)
	got := exceptions(err)
	if len(got) != 3 {
		t.Fatalf("got %d exceptions, want 3: %+v", len(got), got)
	}
	if got[0].Type != "*errors.errorString" || got[0].Value != "db down" {
		t.Errorf("innermost = %+v, want the panic's error", got[0])
	}
	if got[1].Type != "*collage.PanicError" || got[1].Value != "collage: panic: db down" {
		t.Errorf("middle = %+v, want the PanicError", got[1])
	}
	if len(got[1].Frames) == 0 {
		t.Error("the PanicError has no frames")
	}
	if got[2].Type != "*fmt.wrapErrors" || got[2].Frames != nil {
		t.Errorf("outermost = %+v", got[2])
	}
}

func TestExceptions_PanicWithValue(t *testing.T) {
	err := fmt.Errorf("%w: %w", collage.ErrPanic, &collage.PanicError{Value: 42, Stack: []byte("garbage")})
	got := exceptions(err)
	if len(got) != 2 || got[0].Value != "collage: panic: 42" || got[0].Frames != nil {
		t.Errorf("got %+v", got)
	}
}

func TestExceptions_Cycle(t *testing.T) {
	if got := exceptions(&cyclic{}); len(got) != 10 {
		t.Errorf("a self-cycle gave %d exceptions, want 10", len(got))
	}
	if got := exceptions(&cyclicJoin{}); len(got) != 10 {
		t.Errorf("a self-cycling join gave %d exceptions, want 10", len(got))
	}
	if got := exceptions(nil); got != nil {
		t.Errorf("exceptions(nil) = %+v", got)
	}
}

func TestExceptions_JoinFollowsFirst(t *testing.T) {
	err := errors.Join(errors.New("first"), errors.New("second"))
	got := exceptions(err)
	if len(got) != 2 || got[0].Value != "first" {
		t.Errorf("got %+v, want the join then its first branch", got)
	}
}

func TestRequestInfo(t *testing.T) {
	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://example.com/reset/abc?token=s&page=2&token=t", nil)
		r.Header.Set("Cookie", "session=secret-cookie")
		r.Header.Set("Authorization", "Bearer secret-auth")
		r.Header.Set("Proxy-Authorization", "Basic secret-proxy")
		r.Header.Set("X-CSRF-Token", "secret-csrf")
		r.Header.Set("X-Api-Key", "secret-key")
		r.Header.Set("User-Agent", "agent/1")
		r.Header.Set("Referer", "https://x/a?q=1#frag")
		r.RemoteAddr = "203.0.113.9:4321"
		return r
	}
	const pattern = "/reset/{token}"

	got := requestInfo(newReq(), pattern, Options{})
	if got.Method != http.MethodPost {
		t.Errorf("Method = %q", got.Method)
	}
	if got.URL != "http://example.com/reset/{token}" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.Query != "page=[filtered]&token=[filtered]" {
		t.Errorf("Query = %q", got.Query)
	}
	if len(got.Headers) != 2 || got.Headers["User-Agent"] != "agent/1" || got.Headers["Referer"] != "https://x/a" {
		t.Errorf("Headers = %v", got.Headers)
	}
	if got.IP != "" {
		t.Errorf("IP = %q", got.IP)
	}

	all := Options{SendPath: true, SendQuery: true, SendIP: true}
	got = requestInfo(newReq(), pattern, all)
	if got.URL != "http://example.com/reset/abc" {
		t.Errorf("SendPath URL = %q", got.URL)
	}
	if got.Query != "token=s&page=2&token=t" {
		t.Errorf("SendQuery Query = %q", got.Query)
	}
	if got.IP != "203.0.113.9" {
		t.Errorf("SendIP IP = %q", got.IP)
	}

	for _, o := range []Options{{}, all} {
		info := requestInfo(newReq(), pattern, o)
		dump := fmt.Sprintf("%+v", *info)
		if strings.Contains(dump, "secret") {
			t.Errorf("options %+v send a secret header: %s", o, dump)
		}
	}
}

func TestRequestInfo_Referer(t *testing.T) {
	for referer, want := range map[string]string{
		"https://user:pass@x/a?q=1": "https://x/a",
		"https://x/a#frag":          "https://x/a",
		"http://x/%zz?q=1":          "",
		"::not a url":               "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Referer", referer)
		got, sent := requestInfo(r, "", Options{}).Headers["Referer"]
		if got != want || sent != (want != "") {
			t.Errorf("Referer %q: sent %v %q, want %q", referer, sent, got, want)
		}
	}
}

func TestRequestInfo_NoPattern(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://example.com/missing/thing", nil)
	r.TLS = &tls.ConnectionState{}
	r.RemoteAddr = "no-port"
	got := requestInfo(r, "", Options{SendIP: true})
	if got.URL != "https://example.com/missing/thing" {
		t.Errorf("URL = %q, want the raw path when no route resolved", got.URL)
	}
	if got.Query != "" {
		t.Errorf("Query = %q, want empty", got.Query)
	}
	if got.IP != "no-port" {
		t.Errorf("IP = %q", got.IP)
	}
	if requestInfo(nil, "", Options{}) != nil {
		t.Error("requestInfo(nil) is not nil")
	}
}

// testPlugin is a configured plugin that keeps every event.
func testPlugin(o Options) *Plugin {
	if o.Environment == "" {
		o.Environment = "production"
	}
	if o.Release == "" {
		o.Release = "build-1"
	}
	return &Plugin{opts: o, serverName: "host-1", rand: func() float64 { return 0 }}
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestBuild(t *testing.T) {
	route := collage.Route{Kind: "page", Name: "reset", Pattern: "/reset/{token}"}
	r := httptest.NewRequest(http.MethodGet, "http://example.com/reset/abc", nil)

	p := testPlugin(Options{User: func(r *http.Request) User { return User{ID: "u1"} }})
	e, ok := p.event(errors.New("db down"), 500, "data", r, route)
	if !ok {
		t.Fatal("a 500 was dropped")
	}
	if e.Level != "error" {
		t.Errorf("Level = %q", e.Level)
	}
	wantTags := map[string]string{"stage": "data", "route.kind": "page", "method": "GET", "status": "500"}
	if fmt.Sprint(e.Tags) != fmt.Sprint(wantTags) {
		t.Errorf("Tags = %v, want %v", e.Tags, wantTags)
	}
	if e.Transaction != "/reset/{token}" {
		t.Errorf("Transaction = %q", e.Transaction)
	}
	if e.Request == nil || e.Request.URL != "http://example.com/reset/{token}" {
		t.Errorf("Request = %+v", e.Request)
	}
	if e.User == nil || e.User.ID != "u1" {
		t.Errorf("User = %+v", e.User)
	}
	if !hex32.MatchString(e.EventID) {
		t.Errorf("EventID = %q", e.EventID)
	}
	if e.Environment != "production" || e.Release != "build-1" || e.ServerName != "host-1" {
		t.Errorf("Environment %q, Release %q, ServerName %q", e.Environment, e.Release, e.ServerName)
	}
	if e.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
	if len(e.Exceptions) != 1 || e.Exceptions[0].Value != "db down" {
		t.Errorf("Exceptions = %+v", e.Exceptions)
	}

	e2, _ := p.event(errors.New("x"), 500, "data", r, route)
	if e2.EventID == e.EventID {
		t.Error("two events share an ID")
	}

	pe, ok := p.event(panicErr(errors.New("boom")), 500, "panic", r, route)
	if !ok || pe.Level != "fatal" {
		t.Errorf("panic: ok %v, event %+v", ok, pe)
	}
}

func TestBuild_UnknownsOmitted(t *testing.T) {
	p := testPlugin(Options{User: func(*http.Request) User { t.Error("User ran without a request"); return User{} }})
	e, ok := p.event(errors.New("x"), 0, "capture", nil, collage.Route{})
	if !ok {
		t.Fatal("an unknown status was dropped")
	}
	if len(e.Tags) != 1 || e.Tags["stage"] != "capture" {
		t.Errorf("Tags = %v, want only the stage", e.Tags)
	}
	if e.Transaction != "" || e.Request != nil || e.User != nil {
		t.Errorf("Transaction %q, Request %+v, User %+v", e.Transaction, e.Request, e.User)
	}
}

func TestBuild_Context(t *testing.T) {
	p := testPlugin(Options{})
	e, ok := p.build(context.Background(), errors.New("x"), 500, "data", nil)
	if !ok || e.Transaction != "" || e.Tags["route.kind"] != "" {
		t.Errorf("ok %v, event %+v", ok, e)
	}
}

func TestBuild_UserPanics(t *testing.T) {
	p := testPlugin(Options{User: func(*http.Request) User { panic("user lookup") }})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	e, ok := p.event(errors.New("x"), 500, "data", r, collage.Route{})
	if !ok {
		t.Fatal("a panicking User callback dropped the event")
	}
	if e.User != nil {
		t.Errorf("User = %+v, want none", e.User)
	}
}

func TestBuild_MinStatus(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	tests := []struct {
		name   string
		min    int
		err    error
		status int
		keep   bool
	}{
		{"404 under the default", 0, errors.New("x"), 404, false},
		{"499 under the default", 0, errors.New("x"), 499, false},
		{"500 under the default", 0, errors.New("x"), 500, true},
		{"unknown status", 0, errors.New("x"), 0, true},
		{"404 with minStatus 400", 400, errors.New("x"), 404, true},
		{"a panic below minStatus", 0, panicErr(errors.New("x")), 404, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPlugin(Options{MinStatus: tt.min})
			if _, ok := p.event(tt.err, tt.status, "data", r, collage.Route{}); ok != tt.keep {
				t.Errorf("kept %v, want %v", ok, tt.keep)
			}
		})
	}
}

func TestBuild_SampleRate(t *testing.T) {
	p := testPlugin(Options{SampleRate: 0.25})
	for _, tt := range []struct {
		roll float64
		keep bool
	}{{0, true}, {0.2499, true}, {0.25, false}, {0.9, false}} {
		p.rand = func() float64 { return tt.roll }
		if _, ok := p.event(errors.New("x"), 500, "data", nil, collage.Route{}); ok != tt.keep {
			t.Errorf("roll %v: kept %v, want %v", tt.roll, ok, tt.keep)
		}
	}
	all := testPlugin(Options{SampleRate: 1})
	all.rand = func() float64 { t.Error("rand ran at SampleRate 1"); return 0 }
	if _, ok := all.event(errors.New("x"), 500, "data", nil, collage.Route{}); !ok {
		t.Error("SampleRate 1 dropped an event")
	}
	unset := &Plugin{}
	if _, ok := unset.event(errors.New("x"), 500, "data", nil, collage.Route{}); !ok {
		t.Error("an unconfigured plugin dropped a 500")
	}
}

func TestBuild_Cycle(t *testing.T) {
	p := testPlugin(Options{})
	e, ok := p.event(&cyclicJoin{}, 500, "data", nil, collage.Route{})
	if !ok || len(e.Exceptions) != 10 || e.Level != "error" {
		t.Errorf("ok %v, event %+v", ok, e)
	}
}

var password = regexp.MustCompile(`password=\S+`)

func TestBeforeSend_Scrubs(t *testing.T) {
	p := testPlugin(Options{BeforeSend: func(e *Event) bool {
		for i := range e.Exceptions {
			e.Exceptions[i].Value = password.ReplaceAllString(e.Exceptions[i].Value, "password=[filtered]")
		}
		return true
	}})
	err := fmt.Errorf("connect: %w", errors.New("postgres://u@h/db?password=hunter2 refused"))
	e, ok := p.event(err, 500, "data", nil, collage.Route{})
	if !ok {
		t.Fatal("dropped")
	}
	for _, x := range e.Exceptions {
		if strings.Contains(x.Value, "hunter2") {
			t.Errorf("the secret survived BeforeSend: %q", x.Value)
		}
		if !strings.Contains(x.Value, "password=[filtered]") {
			t.Errorf("not scrubbed: %q", x.Value)
		}
	}
}

func TestBeforeSend_Drops(t *testing.T) {
	ran := false
	p := testPlugin(Options{BeforeSend: func(*Event) bool { ran = true; return false }})
	if _, ok := p.event(errors.New("x"), 500, "data", nil, collage.Route{}); ok || !ran {
		t.Errorf("BeforeSend false: kept %v, ran %v", ok, ran)
	}

	low := testPlugin(Options{BeforeSend: func(*Event) bool { t.Error("BeforeSend ran on a filtered event"); return true }})
	low.event(errors.New("x"), 404, "data", nil, collage.Route{})
}

func TestBeforeSend_Panics(t *testing.T) {
	var buf bytes.Buffer
	p := testPlugin(Options{
		DSN:        goodDSN,
		BeforeSend: func(*Event) bool { panic("scrubber broke") },
	})
	p.dsn, _ = parseDSN(goodDSN)
	p.log = slog.New(slog.NewTextHandler(&buf, nil))
	if _, ok := p.event(errors.New("x"), 500, "data", nil, collage.Route{}); ok {
		t.Error("a panicking BeforeSend kept the event")
	}
	out := buf.String()
	if !strings.Contains(out, "scrubber broke") {
		t.Errorf("the panic was not logged: %q", out)
	}
	if strings.Contains(out, "secretkey") {
		t.Errorf("the log carries the DSN: %q", out)
	}
}

func TestBeforeSend_PanicsDefaultLogger(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p := testPlugin(Options{BeforeSend: func(*Event) bool { panic("no logger") }})
	if _, ok := p.event(errors.New("x"), 500, "data", nil, collage.Route{}); ok {
		t.Error("kept")
	}
	if !strings.Contains(buf.String(), "no logger") {
		t.Errorf("not logged through slog.Default: %q", buf.String())
	}
}
