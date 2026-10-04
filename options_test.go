package errortrack

import (
	"context"
	"math"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

const goodDSN = "https://secretkey@o1.ingest.sentry.io/123"

// start builds an app with the plugin and starts it; the error is whichever of
// collage.New (Configure) or Start (Init) came first.
func start(t *testing.T, p *Plugin, dev bool) error {
	t.Helper()
	app, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		DevMode:  dev,
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>hi</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	})
	if err != nil {
		return err
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return app.Start()
}

func TestOptions_Invalid(t *testing.T) {
	t.Setenv("ERRORTRACK_TEST_EMPTY", "")
	tests := map[string]Options{
		"dsn missing":      {},
		"dsnEnv empty":     {DSNEnv: "ERRORTRACK_TEST_EMPTY"},
		"invalid dsn":      {DSN: "https://secretkey@o1.ingest.sentry.io/abc"},
		"sample negative":  {DSN: goodDSN, SampleRate: -0.1},
		"sample above one": {DSN: goodDSN, SampleRate: 1.5},
		"minStatus":        {DSN: goodDSN, MinStatus: -1},
		"perMinute":        {DSN: goodDSN, PerMinute: -1},
		"queueSize":        {DSN: goodDSN, QueueSize: -1},
		"timeout":          {DSN: goodDSN, Timeout: -1},
	}
	for name, o := range tests {
		t.Run(name, func(t *testing.T) {
			err := start(t, New(o), false)
			if err == nil {
				t.Fatal("want a start error")
			}
			if strings.Contains(err.Error(), "secretkey") {
				t.Errorf("error echoes the key: %v", err)
			}
		})
	}
}

func TestOptions_Valid(t *testing.T) {
	if err := start(t, New(Options{DSN: goodDSN, SampleRate: 0.5}), false); err != nil {
		t.Fatal(err)
	}
}

func TestOptions_DSNEnvWins(t *testing.T) {
	t.Setenv("ERRORTRACK_TEST_DSN", "https://envkey@o2.example.com/9")
	p := New(Options{DSN: goodDSN, DSNEnv: "ERRORTRACK_TEST_DSN"})
	if err := start(t, p, false); err != nil {
		t.Fatal(err)
	}
	if p.dsn.Key != "envkey" || p.dsn.Project != "9" {
		t.Errorf("dsn = %+v, want the environment's", p.dsn)
	}
}

func TestOptions_Defaults(t *testing.T) {
	p := New(Options{DSN: goodDSN})
	if err := start(t, p, false); err != nil {
		t.Fatal(err)
	}
	o := p.opts
	if o.Environment != "production" || o.MinStatus != 500 || o.SampleRate != 1 ||
		o.PerMinute != 60 || o.QueueSize != 100 || time.Duration(o.Timeout) != 5*time.Second || p.client == nil {
		t.Errorf("defaults = %+v", o)
	}
	if p.opts.Release == "" {
		t.Error("Release is not filled from the host's BuildID")
	}

	dev := New(Options{DSN: goodDSN})
	if err := start(t, dev, true); err != nil {
		t.Fatal(err)
	}
	if dev.opts.Environment != "development" {
		t.Errorf("Environment = %q in DevMode", dev.opts.Environment)
	}
}

func TestInit_DisabledInDevelopment(t *testing.T) {
	p := New(Options{DSN: goodDSN})
	if err := start(t, p, true); err != nil {
		t.Fatal(err)
	}
	if p.started.Load() {
		t.Error("the sender started in development without InDevelopment")
	}
	on := New(Options{DSN: goodDSN, InDevelopment: true})
	if err := start(t, on, true); err != nil {
		t.Fatal(err)
	}
	if !on.started.Load() {
		t.Error("InDevelopment did not start the sender")
	}
	prod := New(Options{DSN: goodDSN})
	if err := start(t, prod, false); err != nil {
		t.Fatal(err)
	}
	if !prod.started.Load() {
		t.Error("the sender did not start in production")
	}
}

func TestDuration_JSON(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"5s"`)); err != nil || time.Duration(d) != 5*time.Second {
		t.Errorf("string: %v %v", d, err)
	}
	if err := d.UnmarshalJSON([]byte(`1000`)); err != nil || time.Duration(d) != time.Microsecond {
		t.Errorf("number: %v %v", d, err)
	}
	if err := d.UnmarshalJSON([]byte(`"soon"`)); err == nil {
		t.Error("want an error for a bad string")
	}
}

func TestOptions_SampleRateNaN(t *testing.T) {
	o := Options{SampleRate: math.NaN()}
	if err := o.validate(); err == nil {
		t.Fatal("a NaN sampleRate validated")
	}
}

// In development without InDevelopment nothing is sent, so a missing DSN is not
// an error there; a DSN that is given is still checked.
func TestConfigure_DevelopmentNeedsNoDSN(t *testing.T) {
	t.Setenv("ERRORTRACK_TEST_EMPTY", "")
	for name, o := range map[string]Options{
		"no dsn":       {},
		"empty dsnEnv": {DSNEnv: "ERRORTRACK_TEST_EMPTY"},
	} {
		t.Run(name, func(t *testing.T) {
			p := New(o)
			if err := start(t, p, true); err != nil {
				t.Fatalf("development without a DSN: %v", err)
			}
			if p.started.Load() {
				t.Error("the sender started")
			}
		})
	}
	for name, o := range map[string]Options{
		"invalid dsn":                 {DSN: "https://secretkey@o1.ingest.sentry.io/abc"},
		"no dsn, inDevelopment":       {InDevelopment: true},
		"empty dsnEnv, inDevelopment": {DSNEnv: "ERRORTRACK_TEST_EMPTY", InDevelopment: true},
		"invalid sampleRate, no dsn":  {SampleRate: 2},
	} {
		t.Run(name, func(t *testing.T) {
			err := start(t, New(o), true)
			if err == nil {
				t.Fatal("want a start error")
			}
			if strings.Contains(err.Error(), "secretkey") {
				t.Errorf("error echoes the key: %v", err)
			}
		})
	}
}
