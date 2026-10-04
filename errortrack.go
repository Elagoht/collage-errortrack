// Package errortrack sends server errors (5xx and panics) to Sentry, or any
// service speaking the Sentry protocol, with the standard library alone.
package errortrack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name.
const Name = "elagoht/errortrack"

// Plugin is the errortrack plugin.
type Plugin struct {
	opts Options
	dsn  dsn

	disabled   bool // development without InDevelopment: nothing is sent
	host       collage.Host
	log        *slog.Logger
	serverName string
	rand       func() float64 // decides sampling; nil means math/rand/v2's Float64

	now func() time.Time // the sender's clock; nil means time.Now

	senderStarted bool // whether startSender ran
	sender
}

// New returns the plugin.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

// Name returns Name.
func (p *Plugin) Name() string { return Name }

// Version returns the plugin's version.
func (p *Plugin) Version() string { return version }

// Configure reads the configuration, resolves and checks the DSN, and applies
// the defaults. Nothing it returns carries the DSN.
func (p *Plugin) Configure(_ context.Context, host collage.ConfigHost) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	o := &p.opts
	raw := o.DSN
	if o.DSNEnv != "" {
		raw = os.Getenv(o.DSNEnv)
		if raw == "" {
			return fmt.Errorf("errortrack: the environment variable %s is empty", o.DSNEnv)
		}
	}
	if raw == "" {
		return errors.New("errortrack: no DSN: set DSN or DSNEnv")
	}
	d, err := parseDSN(raw)
	if err != nil {
		return err
	}
	p.dsn = d
	if err := o.validate(); err != nil {
		return err
	}

	dev := host.DevMode()
	p.disabled = dev && !o.InDevelopment
	if o.Environment == "" {
		o.Environment = "production"
		if dev {
			o.Environment = "development"
		}
	}
	if o.MinStatus == 0 {
		o.MinStatus = 500
	}
	if o.SampleRate == 0 {
		o.SampleRate = 1
	}
	if o.PerMinute == 0 {
		o.PerMinute = 60
	}
	if o.QueueSize == 0 {
		o.QueueSize = 100
	}
	if o.Timeout == 0 {
		o.Timeout = Duration(5 * time.Second)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: clientTimeout}
	}
	return nil
}

// Init stores the host and logger, and starts the sender unless the plugin is
// disabled (development without InDevelopment).
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if p.dsn.Key == "" {
		return errors.New("errortrack: register the plugin in Config.Plugins, where Configure runs")
	}
	p.host = host
	p.log = host.Logger()
	if p.opts.Release == "" {
		p.opts.Release = host.BuildID()
	}
	if p.disabled {
		return nil
	}
	p.serverName, _ = os.Hostname()
	p.startSender()
	return nil
}

// Shutdown stops accepting events and sends what is queued until ctx ends;
// what is left then is dropped.
func (p *Plugin) Shutdown(ctx context.Context) error { return p.drain(ctx) }

var _ collage.ErrorHook = (*Plugin)(nil)

// OnError reports a failure collage encountered while serving a request. It
// only builds and queues the event, and never fails or blocks the request.
func (p *Plugin) OnError(ctx context.Context, ev *collage.ErrorEvent) error {
	if p.disabled || ev == nil {
		return nil
	}
	if e, ok := p.build(ctx, ev.Err, ev.Status, ev.Stage, ev.Request); ok {
		p.enqueue(e)
	}
	return nil
}

// Capture reports err from outside a request, such as a background job. The
// event has no request and an unknown status, which MinStatus never filters;
// its transaction is the route ctx carries, if any. A nil err, or a disabled
// plugin, does nothing.
func (p *Plugin) Capture(ctx context.Context, err error) {
	if p.disabled || err == nil {
		return
	}
	if e, ok := p.build(ctx, err, 0, "capture", nil); ok {
		p.enqueue(e)
	}
}
