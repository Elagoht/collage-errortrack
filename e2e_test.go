package errortrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// e2eApp is a started collage app with the plugin, a page at /broken whose
// required fragment fails (500) and a panicking /boom, plus its fake.
func e2eApp(t *testing.T, o Options, dev bool) (*collage.App, *Plugin, *fakeSentry) {
	t.Helper()
	fake := newFakeSentry(t)
	o.DSN = strings.Replace(fake.srv.URL, "://", "://fakekey@", 1) + "/123"
	p := New(o)
	app, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		DevMode:  dev,
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>{{.Data}}</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	failing := collage.Load(func(context.Context, *collage.RenderContext) (string, error) {
		return "", errors.New("boom")
	})
	page := collage.NewPage("broken").
		WithContent(collage.NewFragment("body", "p.html").WithDataHandler(failing).Required().Build()).
		WithPath("en", "/broken").Build()
	if err := app.RegisterPage(page); err != nil {
		t.Fatal(err)
	}
	if err := app.Handle("/boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") })); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = app.Shutdown(ctx)
	})
	return app, p, fake
}

// get serves a request through app and returns the recorder.
func get(app *collage.App, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	return w
}

// shutdown drains the plugin so what was queued has reached the fake.
func shutdown(t *testing.T, app *collage.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// decodeEvent is the event JSON of one received envelope.
func decodeEvent(t *testing.T, body []byte) wireEvent {
	t.Helper()
	lines := strings.SplitN(string(body), "\n", 3)
	if len(lines) != 3 {
		t.Fatalf("envelope has %d lines", len(lines))
	}
	var w wireEvent
	if err := json.Unmarshal([]byte(lines[2]), &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestE2E_PageFailureSendsOneEvent(t *testing.T) {
	app, _, fake := e2eApp(t, Options{}, false)
	if w := get(app, "/broken", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("GET /broken = %d, want 500", w.Code)
	}
	shutdown(t, app)
	got := fake.received()
	if len(got) != 1 {
		t.Fatalf("%d envelopes, want 1", len(got))
	}
	ev := decodeEvent(t, got[0].Body)
	if ev.Transaction != "/broken" {
		t.Errorf("Transaction = %q, want /broken", ev.Transaction)
	}
	if ev.Level != "error" || ev.Request == nil || ev.Request.Method != http.MethodGet {
		t.Errorf("level %q, request %+v", ev.Level, ev.Request)
	}
	// The data handler's own error is in the chain, innermost first.
	if ev.Exception == nil || len(ev.Exception.Values) == 0 || ev.Exception.Values[0].Value != "boom" {
		t.Errorf("exception = %+v, want the handler's error innermost", ev.Exception)
	}
}

func TestE2E_PanicIsFatalWithFrames(t *testing.T) {
	app, _, fake := e2eApp(t, Options{}, false)
	if w := get(app, "/boom", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("GET /boom = %d, want 500", w.Code)
	}
	shutdown(t, app)
	got := fake.received()
	if len(got) != 1 {
		t.Fatalf("%d envelopes, want 1", len(got))
	}
	ev := decodeEvent(t, got[0].Body)
	if ev.Level != "fatal" {
		t.Errorf("level = %q, want fatal", ev.Level)
	}
	frames := 0
	if ev.Exception != nil {
		for _, x := range ev.Exception.Values {
			if x.Stacktrace != nil {
				frames += len(x.Stacktrace.Frames)
			}
		}
	}
	if frames == 0 {
		t.Error("the panic event has no frames")
	}
}

func TestE2E_MinStatus(t *testing.T) {
	app, _, fake := e2eApp(t, Options{}, false)
	if w := get(app, "/nowhere", nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET /nowhere = %d, want 404", w.Code)
	}
	shutdown(t, app)
	if n := fake.count(); n != 0 {
		t.Errorf("a 404 sent %d events, want 0", n)
	}

	app, _, fake = e2eApp(t, Options{MinStatus: 400}, false)
	if w := get(app, "/nowhere", nil); w.Code != http.StatusNotFound {
		t.Fatalf("GET /nowhere = %d, want 404", w.Code)
	}
	shutdown(t, app)
	if n := fake.count(); n != 1 {
		t.Errorf("a 404 with MinStatus 400 sent %d events, want 1", n)
	}
}

func TestE2E_DisabledInDevelopment(t *testing.T) {
	app, p, fake := e2eApp(t, Options{}, true)
	get(app, "/broken", nil)
	get(app, "/boom", nil)
	p.Capture(context.Background(), errors.New("job failed"))
	if p.started.Load() {
		t.Error("a sender goroutine started")
	}
	shutdown(t, app)
	if n := fake.count(); n != 0 {
		t.Errorf("%d events sent in development, want 0", n)
	}
}

func TestE2E_Capture(t *testing.T) {
	_, p, fake := e2eApp(t, Options{}, false)
	p.Capture(context.Background(), nil) // a no-op
	p.Capture(context.Background(), errors.New("job failed"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got := fake.received()
	if len(got) != 1 {
		t.Fatalf("%d envelopes, want 1", len(got))
	}
	ev := decodeEvent(t, got[0].Body)
	if ev.Request != nil {
		t.Errorf("request = %+v, want none", ev.Request)
	}
	if ev.Exception == nil || ev.Exception.Values[0].Value != "job failed" {
		t.Errorf("exception = %+v", ev.Exception)
	}
	if ev.Tags["stage"] != "capture" {
		t.Errorf("stage tag = %q, want capture", ev.Tags["stage"])
	}
}

func TestE2E_NoSecretsInEnvelopes(t *testing.T) {
	app, _, fake := e2eApp(t, Options{}, false)
	const cookie, auth = "session=COOKIE-SECRET-8f3a", "Bearer AUTH-SECRET-91bc"
	hdr := map[string]string{"Cookie": cookie, "Authorization": auth, "X-Test": "kept"}
	get(app, "/broken", hdr)
	get(app, "/boom", hdr)
	shutdown(t, app)
	got := fake.received()
	if len(got) != 2 {
		t.Fatalf("%d envelopes, want 2", len(got))
	}
	for _, r := range got {
		for _, secret := range []string{"COOKIE-SECRET-8f3a", "AUTH-SECRET-91bc"} {
			if bytes.Contains(r.Body, []byte(secret)) {
				t.Errorf("an envelope carries %q", secret)
			}
		}
	}
}

// A middleware's panic happens before a route resolves: without SendPath the
// event's URL is only scheme://host, never the raw path.
func TestE2E_MiddlewarePanicSendsNoPath(t *testing.T) {
	for _, sendPath := range []bool{false, true} {
		fake := newFakeSentry(t)
		p := New(Options{DSN: strings.Replace(fake.srv.URL, "://", "://fakekey@", 1) + "/123", SendPath: sendPath})
		app, err := collage.New(&collage.Config{
			Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
			Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>hi</p>`)}}, Root: "t"},
			Plugins:  []collage.Plugin{p},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Use(func(http.Handler) http.Handler {
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("middleware") })
		}); err != nil {
			t.Fatal(err)
		}
		if err := app.Start(); err != nil {
			t.Fatal(err)
		}
		if w := get(app, "/reset/SECRET-TOKEN-77", nil); w.Code != http.StatusInternalServerError {
			t.Fatalf("GET = %d, want 500", w.Code)
		}
		shutdown(t, app)
		got := fake.received()
		if len(got) != 1 {
			t.Fatalf("SendPath %v: %d envelopes, want 1", sendPath, len(got))
		}
		ev := decodeEvent(t, got[0].Body)
		want := "http://example.com"
		if sendPath {
			want += "/reset/SECRET-TOKEN-77"
		}
		if ev.Request == nil || ev.Request.URL != want {
			t.Errorf("SendPath %v: request = %+v, want URL %q", sendPath, ev.Request, want)
		}
		if !sendPath && bytes.Contains(got[0].Body, []byte("SECRET-TOKEN-77")) {
			t.Error("the envelope carries the raw path")
		}
	}
}

// Capture and OnError before the app starts do nothing, and race with nothing
// Init writes.
func TestE2E_CaptureBeforeStart(t *testing.T) {
	fake := newFakeSentry(t)
	p := New(Options{DSN: strings.Replace(fake.srv.URL, "://", "://fakekey@", 1) + "/123"})
	app, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>hi</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Capture(context.Background(), errors.New("too early"))
	_ = p.OnError(context.Background(), &collage.ErrorEvent{Err: errors.New("too early"), Status: 500})

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				p.Capture(context.Background(), errors.New("racing"))
			}
		}
	}()
	startErr := app.Start()
	close(stop)
	<-done
	if startErr != nil {
		t.Fatal(startErr)
	}
	shutdown(t, app)
	for _, r := range fake.received() {
		if bytes.Contains(r.Body, []byte("too early")) {
			t.Error("an event captured before Start was sent")
		}
	}
}
