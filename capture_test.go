package errortrack

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// buildReader reads a finished build, as elagoht/deploy does; with one
// registered, the build asks the handler for every file to capture its headers.
type buildReader struct{ files []collage.BuiltFile }

func (*buildReader) Name() string                             { return "test/buildreader" }
func (*buildReader) Version() string                          { return "0" }
func (*buildReader) Init(context.Context, collage.Host) error { return nil }
func (*buildReader) Shutdown(context.Context) error           { return nil }
func (b *buildReader) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	b.files = ev.Files
	return nil
}

// A static build's header capture is not a reader: a page that renders for the
// build but fails when the build asks for it again answers the capture 500,
// and neither that 500 nor an error its data handler hands Capture reaches
// the tracker.
func TestBuildCaptureIsNotReported(t *testing.T) {
	fake := newFakeSentry(t)
	p := New(Options{DSN: strings.Replace(fake.srv.URL, "://", "://fakekey@", 1) + "/123"})
	reader := &buildReader{}
	app, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		Logger:   slog.New(slog.DiscardHandler),
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>{{.}}</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p, reader},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fails only when the build asks for the page: the build's own render
	// succeeds, so the file is written and then captured.
	failing := collage.Load(func(ctx context.Context, _ *collage.RenderContext) (string, error) {
		if collage.IsCapture(ctx) {
			return "", errors.New("boom")
		}
		return "ok", nil
	})
	// Hands an error to Capture when the build asks for the page, as a data
	// handler reporting a failure it recovers from would.
	capturing := collage.Load(func(ctx context.Context, _ *collage.RenderContext) (string, error) {
		if collage.IsCapture(ctx) {
			p.Capture(ctx, errors.New("recovered"))
		}
		return "ok", nil
	})
	for _, page := range []*collage.Page{
		collage.NewPage("broken").WithContent(collage.NewFragment("body", "p.html").WithData(failing).Required().Build()).WithPath("en", "/broken").Static().Build(),
		collage.NewPage("recovers").WithContent(collage.NewFragment("body", "p.html").WithData(capturing).Build()).WithPath("en", "/recovers").Static().Build(),
	} {
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	builder, err := collage.NewBuilder(app, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The capture's 500 is the build's finding, not the tracker's event.
	answered500 := false
	for _, f := range report.Findings {
		if f.Rule == "capture-status" && f.Path == "/broken" && strings.Contains(f.Message, "500") {
			answered500 = true
			continue
		}
		t.Errorf("finding: %s %s: %s", f.Rule, f.Path, f.Message)
	}
	if !answered500 {
		t.Error("no capture-status finding for /broken: the capture did not fail")
	}
	captured := map[string]int{}
	for _, f := range reader.files {
		if f.Captured {
			captured[f.Path] = f.Status
		}
	}
	if captured["/broken"] != http.StatusInternalServerError || captured["/recovers"] != http.StatusOK {
		t.Fatalf("captured %v, want /broken 500 and /recovers 200", captured)
	}
	// Drain the sender, so anything queued has reached the fake.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.received(); len(got) != 0 {
		for _, r := range got {
			t.Errorf("sent: %s", r.Body)
		}
	}
}
