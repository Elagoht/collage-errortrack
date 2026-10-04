package errortrack

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// dsn is a parsed Sentry DSN. Its key is a credential: nothing here prints it.
type dsn struct {
	Key     string
	Host    string // with the port, when there is one
	Scheme  string
	Prefix  string // the path before the project, with a trailing "/", or ""
	Project string
}

// parseDSN reads https://<key>@<host>[:port]/[<prefix>/]<project>. Its errors
// never carry the DSN or any part of it, the url package's own included.
func parseDSN(raw string) (dsn, error) {
	if raw == "" {
		return dsn{}, errors.New("errortrack: the DSN is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return dsn{}, errors.New("errortrack: the DSN is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return dsn{}, errors.New("errortrack: the DSN scheme must be http or https")
	}
	key := u.User.Username()
	if key == "" {
		return dsn{}, errors.New("errortrack: the DSN has no key (https://<key>@<host>/<project>)")
	}
	if u.Host == "" {
		return dsn{}, errors.New("errortrack: the DSN has no host")
	}
	path := strings.Trim(u.Path, "/")
	prefix, project := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		prefix, project = path[:i+1], path[i+1:]
	}
	if project == "" {
		return dsn{}, errors.New("errortrack: the DSN has no project")
	}
	if n, err := strconv.ParseUint(project, 10, 64); err != nil || n == 0 {
		return dsn{}, errors.New("errortrack: the DSN project must be a number")
	}
	return dsn{Key: key, Host: u.Host, Scheme: u.Scheme, Prefix: prefix, Project: project}, nil
}

// endpoint is where an event envelope is POSTed.
func (d dsn) endpoint() string {
	return d.Scheme + "://" + d.Host + "/" + d.Prefix + "api/" + d.Project + "/envelope/"
}

// auth is the X-Sentry-Auth header's value.
func (d dsn) auth(version string) string {
	return "Sentry sentry_version=7, sentry_key=" + d.Key + ", sentry_client=collage-errortrack/" + version
}
