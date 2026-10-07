package errortrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultPerMinute  = 60
	defaultQueueSize  = 100
	defaultTimeout    = 5 * time.Second
	defaultPause      = time.Minute // a 429 that says nothing of how long
	maxResponseBody   = 64 << 10
	dropReportEvery   = time.Minute
	envelopeType      = "application/x-sentry-envelope"
	sdkName           = "collage-errortrack"
	version           = "0.1.4"
	envelopeTimestamp = time.RFC3339Nano
)

// sender is the queue and what its goroutine keeps. The zero value is a
// sender that never started: enqueue and drain do nothing.
type sender struct {
	mu     sync.RWMutex // held to read closed and send, and to close queue
	queue  chan *Event
	closed bool
	done   chan struct{} // closed when the goroutine returns

	sendCtx    context.Context // cancelled when Shutdown's context ends
	cancelSend context.CancelFunc
	client     *http.Client

	dropped atomic.Int64 // every event dropped since the start

	reportMu    sync.Mutex
	reported    int64     // dropped when the last report was logged
	lastReport  time.Time // when it was
	everLogged  bool      // whether a report was ever logged
	sendWindow  []time.Time
	pausedUntil time.Time // the goroutine's alone, as is sendWindow
}

// clock is p.now, or time.Now.
func (p *Plugin) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// startSender makes the queue and starts the one goroutine draining it.
func (p *Plugin) startSender() {
	size := p.opts.QueueSize
	if size == 0 {
		size = defaultQueueSize
	}
	p.client = sendClient(p.opts.HTTPClient)
	p.sendCtx, p.cancelSend = context.WithCancel(context.Background())
	p.done = make(chan struct{})
	p.queue = make(chan *Event, size)
	go p.run(p.queue)
	p.started.Store(true)
}

// run sends every queued event until the queue is closed and empty.
func (p *Plugin) run(queue <-chan *Event) {
	defer close(p.done)
	for e := range queue {
		p.deliver(e)
		p.reportDrops()
	}
	p.reportDropsNow()
}

// enqueue queues e for sending. It never blocks: when the queue is full, or
// after Shutdown, e is dropped and counted. Without a sender it does nothing.
func (p *Plugin) enqueue(e *Event) {
	if p.queue == nil {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		p.dropped.Add(1)
		return
	}
	select {
	case p.queue <- e:
	default:
		p.dropped.Add(1)
		p.reportDrops() // the sender may be stuck on a hanging Sentry
	}
}

// drain stops accepting events and waits until the queue is sent or ctx ends.
// When ctx ends first, the send in flight is cancelled, what is left is
// dropped, and ctx's error is returned.
func (p *Plugin) drain(ctx context.Context) error {
	if p.queue == nil {
		return nil
	}
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.queue)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
		p.cancelSend()
		return nil
	case <-ctx.Done():
		p.cancelSend()
		return fmt.Errorf("errortrack: events were left unsent: %w", ctx.Err())
	}
}

// deliver sends one event, unless sending is cancelled, paused by a 429, or at
// PerMinute for the last minute; each of those drops and counts it. A failed
// send is logged and counted too. Only the sender's goroutine calls it.
func (p *Plugin) deliver(e *Event) {
	ctx := p.sendCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		p.dropped.Add(1)
		return
	}
	now := p.clock()
	if now.Before(p.pausedUntil) || !p.admit(now) {
		p.dropped.Add(1)
		return
	}
	if !p.post(ctx, e, now) {
		p.dropped.Add(1)
	}
}

// admit records a send at now if fewer than PerMinute were sent in the minute
// before it, and reports whether it did.
func (p *Plugin) admit(now time.Time) bool {
	limit := p.opts.PerMinute
	if limit == 0 {
		limit = defaultPerMinute
	}
	w := p.sendWindow[:0]
	for _, t := range p.sendWindow {
		if now.Sub(t) < time.Minute {
			w = append(w, t)
		}
	}
	p.sendWindow = w
	if len(w) >= limit {
		return false
	}
	p.sendWindow = append(w, now)
	return true
}

// post POSTs e's envelope and reports whether Sentry accepted it. A 429 pauses
// sending; a failure is logged with the host alone, never the DSN or its key.
func (p *Plugin) post(ctx context.Context, e *Event, now time.Time) bool {
	host := p.dsn.Host
	body, err := envelope(e, now)
	if err != nil {
		p.logger().Error("errortrack: an event could not be encoded; it is dropped", slog.String("reason", transportReason(err)))
		return false
	}
	timeout := time.Duration(p.opts.Timeout)
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.dsn.endpoint(), bytes.NewReader(body))
	if err != nil {
		p.logger().Error("errortrack: sending an event failed", slog.String("host", host), slog.String("reason", "the request could not be made"))
		return false
	}
	req.Header.Set("X-Sentry-Auth", p.dsn.auth(p.Version()))
	req.Header.Set("Content-Type", envelopeType)
	client := p.client
	if client == nil {
		client = sendClient(p.opts.HTTPClient)
	}
	resp, err := client.Do(req)
	if err != nil {
		p.logger().Error("errortrack: sending an event failed", slog.String("host", host), slog.String("reason", transportReason(err)))
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		pause := retryAfter(resp.Header, now)
		p.pausedUntil = now.Add(pause)
		p.logger().Warn("errortrack: Sentry is rate limiting; sending is paused",
			slog.String("host", host), slog.Duration("pause", pause))
		return false
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		p.logger().Error("errortrack: sending an event failed", slog.String("host", host), slog.Int("status", resp.StatusCode))
		return false
	}
	return true
}

// sendClient is a copy of c, or of a client with no timeout of its own (each
// send is bounded by Timeout), that never follows a redirect: following one
// would carry X-Sentry-Auth to wherever it points. A 3xx is a failed send.
func sendClient(c *http.Client) *http.Client {
	var cp http.Client
	if c != nil {
		cp = *c
	}
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

// transportReason is err without the URL a *url.Error carries.
func transportReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return err.Error()
}

// retryAfter is how long a 429 pauses sending: the longest window of
// X-Sentry-Rate-Limits, else Retry-After in seconds or as an HTTP date, else
// a minute.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v := h.Get("X-Sentry-Rate-Limits"); v != "" {
		var longest int64
		found := false
		for _, entry := range strings.Split(v, ",") {
			secs, _, _ := strings.Cut(strings.TrimSpace(entry), ":")
			if n, err := strconv.ParseInt(secs, 10, 64); err == nil && n >= 0 {
				found = true
				longest = max(longest, n)
			}
		}
		if found {
			return seconds(longest)
		}
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return seconds(n)
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(now), 0)
		}
	}
	return defaultPause
}

// seconds is n seconds, capped at a day so a wild header cannot overflow.
func seconds(n int64) time.Duration {
	const day = 24 * 60 * 60
	return time.Duration(min(n, day)) * time.Second
}

// reportDrops logs the events dropped since the last report, at most once a
// minute by p.clock.
func (p *Plugin) reportDrops() { p.report(false) }

// reportDropsNow logs the events dropped since the last report, whenever.
func (p *Plugin) reportDropsNow() { p.report(true) }

func (p *Plugin) report(now bool) {
	p.reportMu.Lock()
	defer p.reportMu.Unlock()
	total := p.dropped.Load()
	n := total - p.reported
	if n <= 0 {
		return
	}
	t := p.clock()
	if !now && p.everLogged && t.Sub(p.lastReport) < dropReportEvery {
		return
	}
	p.reported, p.lastReport, p.everLogged = total, t, true
	p.logger().Warn(fmt.Sprintf("errortrack: dropped %d events", n), slog.Int64("count", n))
}

// Wire format: Sentry's envelope, with the keys it reads.
type (
	envelopeHeader struct {
		EventID string `json:"event_id"`
		SentAt  string `json:"sent_at"`
	}
	itemHeader struct {
		Type   string `json:"type"`
		Length int    `json:"length"`
	}
	wireEvent struct {
		EventID     string            `json:"event_id"`
		Timestamp   string            `json:"timestamp,omitempty"`
		Platform    string            `json:"platform"`
		Level       string            `json:"level,omitempty"`
		Environment string            `json:"environment,omitempty"`
		Release     string            `json:"release,omitempty"`
		ServerName  string            `json:"server_name,omitempty"`
		Transaction string            `json:"transaction,omitempty"`
		Tags        map[string]string `json:"tags,omitempty"`
		User        *wireUser         `json:"user,omitempty"`
		Request     *wireRequest      `json:"request,omitempty"`
		Exception   *wireExceptions   `json:"exception,omitempty"`
		SDK         wireSDK           `json:"sdk"`
	}
	wireUser struct {
		ID        string `json:"id,omitempty"`
		Username  string `json:"username,omitempty"`
		Email     string `json:"email,omitempty"`
		IPAddress string `json:"ip_address,omitempty"`
	}
	wireRequest struct {
		Method      string            `json:"method,omitempty"`
		URL         string            `json:"url,omitempty"`
		QueryString string            `json:"query_string,omitempty"`
		Headers     map[string]string `json:"headers,omitempty"`
	}
	wireExceptions struct {
		Values []wireException `json:"values"`
	}
	wireException struct {
		Type       string          `json:"type,omitempty"`
		Value      string          `json:"value,omitempty"`
		Stacktrace *wireStacktrace `json:"stacktrace,omitempty"`
	}
	wireStacktrace struct {
		Frames []wireFrame `json:"frames"`
	}
	wireFrame struct {
		Function string `json:"function,omitempty"`
		Filename string `json:"filename,omitempty"`
		Lineno   int    `json:"lineno,omitempty"`
		InApp    bool   `json:"in_app"`
	}
	wireSDK struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
)

// envelope is e as a Sentry envelope: a header line, an item header line and
// the event JSON, with no trailing newline. now is the sent_at time. The
// request's IP, when sent, goes in user.ip_address, where Sentry reads it.
func envelope(e *Event, now time.Time) ([]byte, error) {
	w := wireEvent{
		EventID:     e.EventID,
		Platform:    "go",
		Level:       e.Level,
		Environment: e.Environment,
		Release:     e.Release,
		ServerName:  e.ServerName,
		Transaction: e.Transaction,
		Tags:        e.Tags,
		SDK:         wireSDK{Name: sdkName, Version: version},
	}
	if !e.Timestamp.IsZero() {
		w.Timestamp = e.Timestamp.UTC().Format(envelopeTimestamp)
	}
	if e.User != nil {
		w.User = &wireUser{ID: e.User.ID, Username: e.User.Username, Email: e.User.Email}
	}
	if r := e.Request; r != nil {
		w.Request = &wireRequest{Method: r.Method, URL: r.URL, QueryString: r.Query, Headers: r.Headers}
		if r.IP != "" {
			if w.User == nil {
				w.User = &wireUser{}
			}
			w.User.IPAddress = r.IP
		}
	}
	if len(e.Exceptions) > 0 {
		w.Exception = &wireExceptions{Values: make([]wireException, 0, len(e.Exceptions))}
		for _, x := range e.Exceptions {
			wx := wireException{Type: x.Type, Value: x.Value}
			if len(x.Frames) > 0 {
				wx.Stacktrace = &wireStacktrace{Frames: make([]wireFrame, 0, len(x.Frames))}
				for _, f := range x.Frames {
					wx.Stacktrace.Frames = append(wx.Stacktrace.Frames,
						wireFrame{Function: f.Function, Filename: f.File, Lineno: f.Line, InApp: f.InApp})
				}
			}
			w.Exception.Values = append(w.Exception.Values, wx)
		}
	}

	event, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	header, err := json.Marshal(envelopeHeader{EventID: e.EventID, SentAt: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	item, err := json.Marshal(itemHeader{Type: "event", Length: len(event)})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Grow(len(header) + len(item) + len(event) + 2)
	b.Write(header)
	b.WriteByte('\n')
	b.Write(item)
	b.WriteByte('\n')
	b.Write(event)
	return b.Bytes(), nil
}
