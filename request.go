package errortrack

import (
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// sentHeaders are the only request headers an event carries. Cookie,
// Authorization, CSRF tokens and every other header are never sent.
var sentHeaders = []string{"User-Agent", "Accept", "Accept-Language", "Content-Type", "Content-Length", "Referer"}

// requestInfo describes r with the defaults of the Privacy table: the route
// pattern for the path (no path at all, only scheme://host, when no route
// resolved), query keys with filtered values, an allowlist of headers with the
// Referer cut to its origin, and no client address. SendPath sends the raw path
// and the Referer's path. Nil when r is nil.
func requestInfo(r *http.Request, pattern string, o Options) *RequestInfo {
	if r == nil {
		return nil
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	path := pattern
	if o.SendPath {
		path = r.URL.Path
	}
	info := &RequestInfo{
		Method:  r.Method,
		URL:     scheme + "://" + r.Host + path,
		Headers: map[string]string{},
	}
	if o.SendQuery {
		info.Query = r.URL.RawQuery
	} else {
		info.Query = filteredQuery(r.URL.Query())
	}
	for _, name := range sentHeaders {
		v := r.Header.Get(name)
		if v == "" {
			continue
		}
		if name == "Referer" {
			var ok bool
			if v, ok = bareReferer(v, o.SendPath); !ok {
				continue
			}
		}
		info.Headers[name] = v
	}
	if o.SendIP {
		info.IP = r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			info.IP = host
		}
	}
	return info
}

// filteredQuery lists q's keys, sorted and each once, with "[filtered]" for
// every value.
func filteredQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = url.QueryEscape(k) + "=[filtered]"
	}
	return strings.Join(parts, "&")
}

// bareReferer cuts a Referer to its origin, scheme://host, or with keepPath to
// its scheme, host and path, dropping credentials, query and fragment. One that
// cannot be parsed, or has no scheme or host, is not sent at all: cutting it by
// hand could leave a secret in.
func bareReferer(raw string, keepPath bool) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	if !keepPath {
		return u.Scheme + "://" + u.Host, true
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String(), true
}
