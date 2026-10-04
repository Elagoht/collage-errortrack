package errortrack

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// maxChain is how many links of an error chain become exceptions at most, which
// also ends a chain whose Unwrap cycles.
const maxChain = 10

// build turns a failure into an event, or reports false when it is dropped: a
// non-panic with a known status below MinStatus, an event outside the sample,
// or one BeforeSend refused. Whether the plugin is enabled is not its concern.
func (p *Plugin) build(ctx context.Context, err error, status int, stage string, r *http.Request) (*Event, bool) {
	return p.event(err, status, stage, r, collage.RouteInfo(ctx))
}

// event is build once the route is known.
func (p *Plugin) event(err error, status int, stage string, r *http.Request, route collage.Route) (*Event, bool) {
	o := &p.opts
	panicked := isPanic(err)
	minStatus := o.MinStatus
	if minStatus == 0 {
		minStatus = 500
	}
	if !panicked && status != 0 && status < minStatus {
		return nil, false
	}
	if rate := o.SampleRate; rate > 0 && rate < 1 {
		roll := p.rand
		if roll == nil {
			roll = mrand.Float64
		}
		if roll() >= rate {
			return nil, false
		}
	}

	e := &Event{
		EventID:     eventID(),
		Timestamp:   time.Now().UTC(),
		Level:       "error",
		Exceptions:  exceptions(err),
		Tags:        map[string]string{"stage": stage},
		Request:     requestInfo(r, route.Pattern, *o),
		Environment: o.Environment,
		Release:     o.Release,
		ServerName:  p.serverName,
		Transaction: route.Pattern,
	}
	if panicked {
		e.Level = "fatal"
	}
	if route.Kind != "" {
		e.Tags["route.kind"] = route.Kind
	}
	if r != nil && r.Method != "" {
		e.Tags["method"] = r.Method
	}
	if status != 0 {
		e.Tags["status"] = strconv.Itoa(status)
	}
	if o.User != nil && r != nil {
		e.User = p.user(r)
	}
	if o.BeforeSend != nil && !p.beforeSend(e) {
		return nil, false
	}
	return e, true
}

// user runs the User callback; a panic in it leaves the event without a user.
func (p *Plugin) user(r *http.Request) (u *User) {
	defer func() {
		if v := recover(); v != nil {
			u = nil
			p.logger().Error("errortrack: the User callback panicked; the event is sent without a user",
				slog.String("panic", fmt.Sprint(v)))
		}
	}()
	got := p.opts.User(r)
	return &got
}

// beforeSend runs BeforeSend; a panic in it drops the event and is logged.
func (p *Plugin) beforeSend(e *Event) (keep bool) {
	defer func() {
		if v := recover(); v != nil {
			keep = false
			p.logger().Error("errortrack: BeforeSend panicked; the event is dropped",
				slog.String("panic", fmt.Sprint(v)))
		}
	}()
	return p.opts.BeforeSend(e)
}

// logger is the host's logger from Init, or slog's default before it.
func (p *Plugin) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// eventID is 32 lowercase hex characters from crypto/rand.
func eventID() string {
	var b [16]byte
	_, _ = crand.Read(b[:]) // never fails since Go 1.24
	return hex.EncodeToString(b[:])
}

// exceptions walks err's chain, outermost first, following the first branch of
// a multi-error, unless a later branch holds the panic: the core reports one as
// fmt.Errorf("%w: %w", ErrPanic, &PanicError{...}), whose first branch is the
// bare sentinel. Each link becomes one exception with its %T type and its own
// message; a *PanicError, typed by its public name, gets frames from its stack. They are returned
// outermost last, as Sentry expects, and at most maxChain of them.
func exceptions(err error) []Exception {
	var out []Exception
	for err != nil && len(out) < maxChain {
		x := Exception{Type: fmt.Sprintf("%T", err), Value: err.Error()}
		if pe, ok := err.(*collage.PanicError); ok {
			// The alias's %T names the internal package; report the public name.
			x.Type = "*collage.PanicError"
			x.Frames = parseFrames(pe.Stack, mainModule())
		}
		out = append(out, x)
		err = next(err)
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// next is the link after err: its Unwrap, or for a multi-error the first branch
// holding a panic, else its first branch.
func next(err error) error {
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		return u.Unwrap()
	case interface{ Unwrap() []error }:
		branches := u.Unwrap()
		for _, b := range branches {
			if hasPanic(b) {
				return b
			}
		}
		for _, b := range branches {
			if b != nil {
				return b
			}
		}
	}
	return nil
}

// isPanic reports whether err is a panic: it holds a *PanicError or ErrPanic.
// Unlike errors.As and errors.Is, it ends on a cyclic chain.
func isPanic(err error) bool {
	return search(err, func(e error) bool {
		_, ok := e.(*collage.PanicError)
		return ok || e == collage.ErrPanic
	})
}

// hasPanic reports whether err holds a *PanicError.
func hasPanic(err error) bool {
	return search(err, func(e error) bool {
		_, ok := e.(*collage.PanicError)
		return ok
	})
}

// search reports whether any error in err's tree, every branch of a
// multi-error included, matches. It visits at most maxChain*maxChain errors, so
// a cycle or a vast tree ends.
func search(err error, match func(error) bool) bool {
	budget := maxChain * maxChain
	var walk func(error) bool
	walk = func(e error) bool {
		for e != nil {
			if budget--; budget < 0 {
				return false
			}
			if match(e) {
				return true
			}
			switch u := e.(type) {
			case interface{ Unwrap() error }:
				e = u.Unwrap()
			case interface{ Unwrap() []error }:
				for _, b := range u.Unwrap() {
					if walk(b) {
						return true
					}
				}
				return false
			default:
				return false
			}
		}
		return false
	}
	return walk(err)
}
