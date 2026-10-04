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
// pattern for the path (the raw path when no route resolved), query keys with
// filtered values, an allowlist of headers, and no client address. Nil when r
// is nil.
func requestInfo(r *http.Request, pattern string, o Options) *RequestInfo {
	if r == nil {
		return nil
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	path := pattern
	if o.SendPath || path == "" {
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
			if v, ok = bareReferer(v); !ok {
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

// bareReferer drops a Referer's credentials, query and fragment. An unparsable
// one is not sent at all: cutting it by hand could leave a secret in.
func bareReferer(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String(), true
}
