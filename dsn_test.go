package errortrack

import (
	"strings"
	"testing"
)

func TestParseDSN_Valid(t *testing.T) {
	tests := map[string]struct {
		raw                        string
		key, host, prefix, project string
		endpoint                   string
	}{
		"sentry.io": {
			raw: "https://abc@o1.ingest.sentry.io/123", key: "abc", host: "o1.ingest.sentry.io", project: "123",
			endpoint: "https://o1.ingest.sentry.io/api/123/envelope/",
		},
		"port and prefix": {
			raw: "https://abc@sentry.example.com:9000/prefix/7", key: "abc", host: "sentry.example.com:9000", prefix: "prefix/", project: "7",
			endpoint: "https://sentry.example.com:9000/prefix/api/7/envelope/",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d, err := parseDSN(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if d.Key != tt.key || d.Host != tt.host || d.Prefix != tt.prefix || d.Project != tt.project || d.Scheme != "https" {
				t.Errorf("got %+v", d)
			}
			if got := d.endpoint(); got != tt.endpoint {
				t.Errorf("endpoint = %q, want %q", got, tt.endpoint)
			}
		})
	}
}

func TestParseDSN_Invalid(t *testing.T) {
	tests := map[string]string{
		"no key":        "https://o1.ingest.sentry.io/123",
		"empty key":     "https://@o1.ingest.sentry.io/123",
		"no project":    "https://secretkey@o1.ingest.sentry.io",
		"empty project": "https://secretkey@o1.ingest.sentry.io/",
		"non-numeric":   "https://secretkey@o1.ingest.sentry.io/abc",
		"ftp scheme":    "ftp://secretkey@o1.ingest.sentry.io/123",
		"empty":         "",
		"no host":       "https://secretkey@/123",
		"not a url":     "://secretkey@%zz/1",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseDSN(raw)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "secretkey") {
				t.Errorf("error echoes the key: %v", err)
			}
		})
	}
}

func TestDSN_Auth(t *testing.T) {
	d, err := parseDSN("https://abc@o1.ingest.sentry.io/123")
	if err != nil {
		t.Fatal(err)
	}
	want := "Sentry sentry_version=7, sentry_key=abc, sentry_client=collage-errortrack/0.1.0"
	if got := d.auth("0.1.0"); got != want {
		t.Errorf("auth = %q, want %q", got, want)
	}
}
