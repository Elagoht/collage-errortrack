package errortrack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Duration is a time.Duration that reads from configuration as Go writes one,
// "5s", as well as a number of nanoseconds.
type Duration time.Duration

// UnmarshalJSON reads "5s" or a number of nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("errortrack: %w", err)
		}
		*d = Duration(v)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("errortrack: a duration is a string like \"5s\" or a number of nanoseconds")
	}
	*d = Duration(n)
	return nil
}

// Options configures the plugin.
type Options struct {
	DSNEnv        string   `json:"dsnEnv"`        // name of the environment variable holding the DSN
	DSN           string   `json:"-"`             // or set from Go
	Environment   string   `json:"environment"`   // default "development" in DevMode, else "production"
	Release       string   `json:"release"`       // default host.BuildID()
	MinStatus     int      `json:"minStatus"`     // default 500; panics are always sent
	SampleRate    float64  `json:"sampleRate"`    // fraction sent, (0, 1]; 0 (unset) means 1
	PerMinute     int      `json:"perMinute"`     // events sent per minute at most, default 60
	QueueSize     int      `json:"queueSize"`     // default 100
	InDevelopment bool     `json:"inDevelopment"` // default false: nothing is sent in development
	Timeout       Duration `json:"timeout"`       // per send, default "5s"
	SendPath      bool     `json:"sendPath"`      // raw path instead of the route pattern, and the Referer's path
	SendQuery     bool     `json:"sendQuery"`     // query values instead of "[filtered]"
	SendIP        bool     `json:"sendIP"`        // the client address

	User       func(r *http.Request) User `json:"-"`
	BeforeSend func(e *Event) bool        `json:"-"` // false drops the event
	HTTPClient *http.Client               `json:"-"`
}

// User identifies the reader an event happened to.
type User struct{ ID, Username, Email string }

// Event is what is sent, in the shape the plugin builds and BeforeSend may edit.
type Event struct {
	EventID     string
	Timestamp   time.Time
	Level       string      // "error", or "fatal" for a panic
	Exceptions  []Exception // outermost last, as Sentry expects
	Tags        map[string]string
	Request     *RequestInfo
	User        *User
	Environment string
	Release     string
	ServerName  string // os.Hostname(), or "" when it fails
	Transaction string // the route pattern
}

// Exception is one link of an error chain.
type Exception struct {
	Type, Value string
	Frames      []Frame // only for a panic
}

// Frame is one line of a panic's stack.
type Frame struct {
	Function, File string
	Line           int
	InApp          bool
}

// RequestInfo is what an event says about the request.
type RequestInfo struct {
	Method, URL, Query string
	Headers            map[string]string
	IP                 string
}

// validate checks the options. Its errors never carry the DSN.
func (o *Options) validate() error {
	switch {
	case !(o.SampleRate >= 0 && o.SampleRate <= 1): // NaN fails both
		return fmt.Errorf("errortrack: sampleRate %v must be in (0, 1], or 0 for all", o.SampleRate)
	case o.MinStatus < 0:
		return fmt.Errorf("errortrack: minStatus %d must not be negative", o.MinStatus)
	case o.PerMinute < 0:
		return fmt.Errorf("errortrack: perMinute %d must not be negative", o.PerMinute)
	case o.QueueSize < 0:
		return fmt.Errorf("errortrack: queueSize %d must not be negative", o.QueueSize)
	case o.Timeout < 0:
		return fmt.Errorf("errortrack: timeout must not be negative")
	}
	return nil
}
