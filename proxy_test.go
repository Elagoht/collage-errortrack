package errortrack

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Elagoht/collage/pkg/collage"
	"github.com/Elagoht/collage/pkg/collagetest"
)

// proxiedSite is a site behind the trusted proxy 10.0.0.1 with the plugin and
// a page at /broken whose required fragment fails, and its fake Sentry.
func proxiedSite(t *testing.T, o Options) (*collagetest.Site, *fakeSentry) {
	t.Helper()
	fake := newFakeSentry(t)
	o.DSN = strings.Replace(fake.srv.URL, "://", "://fakekey@", 1) + "/123"
	failing := collage.Load(func(context.Context, *collage.RenderContext) (string, error) {
		return "", errors.New("boom")
	})
	site := collagetest.NewSite(t,
		collagetest.WithPlugins(New(o)),
		collagetest.WithTemplates(map[string]string{"p.html": `<p>{{.}}</p>`}),
		collagetest.WithRegister(collage.NewPage("broken").
			WithContent(collage.NewFragment("body", "p.html").WithData(failing).Required().Build()).
			WithPath("en", "/broken").Build()),
		collagetest.WithConfig(func(c *collage.Config) { c.Server.TrustedProxies = []string{"10.0.0.1"} }),
	)
	return site, fake
}

// Behind a trusted proxy, sendIP sends the reader's address (collage.ClientIP),
// not the proxy's, and the URL's scheme is the one the proxy says the reader
// used (collage.IsHTTPS).
func TestE2E_BehindTrustedProxy(t *testing.T) {
	site, fake := proxiedSite(t, Options{SendIP: true})
	req := site.Request(http.MethodGet, "/broken", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	site.Do(req).WantStatus(http.StatusInternalServerError)
	shutdown(t, site.App)
	got := fake.received()
	if len(got) != 1 {
		t.Fatalf("%d envelopes, want 1", len(got))
	}
	ev := decodeEvent(t, got[0].Body)
	if ev.User == nil || ev.User.IPAddress != "203.0.113.9" {
		t.Errorf("user = %+v, want ip_address 203.0.113.9 (the reader, not the proxy)", ev.User)
	}
	if ev.Request == nil || !strings.HasPrefix(ev.Request.URL, "https://") {
		t.Errorf("request = %+v, want an https:// URL", ev.Request)
	}
	if ev.Tags["route.kind"] != collage.RouteKindPage || ev.Tags["stage"] == "" {
		t.Errorf("tags = %v", ev.Tags)
	}
}

// The duration "timeout" refuses a number, which used to be nanoseconds.
func TestConfigure_TimeoutNumberRefused(t *testing.T) {
	_, err := collagetest.TrySite(t,
		collagetest.WithPlugins(New(Options{DSN: goodDSN})),
		collagetest.WithPluginConfig(Name, `{"timeout":5}`),
	)
	if err == nil || !strings.Contains(err.Error(), "as a string") {
		t.Errorf("err = %v, want a duration-as-string error", err)
	}
}
