package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseVersionInfo(t *testing.T) {
	v, err := parseVersionInfo(`{"Version":"v0.1.0","Time":"2026-09-19T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "v0.1.0" || v.Time != "2026-09-19T00:00:00Z" {
		t.Errorf("got %+v", v)
	}
}

func TestParseVersionInfo_Invalid(t *testing.T) {
	if _, err := parseVersionInfo("not json"); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestCheckVersion(t *testing.T) {
	cases := []struct {
		name string
		r    report
		want string
	}{
		{"literal version, no resolution", report{version: "v1.2.3"}, "v1.2.3"},
		{"latest resolved", report{version: "latest", resolvedVersion: "v1.2.3"}, "v1.2.3"},
	}
	for _, c := range cases {
		if got := c.r.checkVersion(); got != c.want {
			t.Errorf("%s: checkVersion() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestProbe_LatestQuery_UnknownModule confirms probe() doesn't try to
// resolve "latest" when the module itself is unknown (@latest 404s) — it
// must fall through to probing the literal "latest" string like any other
// unresolvable version, not panic or resolve to an empty version.
func TestProbe_LatestQuery_UnknownModule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe("example.com/nope", "latest")
	if r.resolvedVersion != "" {
		t.Errorf("resolvedVersion = %q, want empty for an unknown module", r.resolvedVersion)
	}
	if r.moduleKnown() {
		t.Error("moduleKnown() = true, want false")
	}
}

func TestReady(t *testing.T) {
	cases := []struct {
		name string
		r    report
		want bool
	}{
		{"both ok", report{versionInfo: probeResult{ok: true}, sum: probeResult{ok: true}}, true},
		{"versionInfo not ok", report{versionInfo: probeResult{ok: false}, sum: probeResult{ok: true}}, false},
		{"sum not ok", report{versionInfo: probeResult{ok: true}, sum: probeResult{ok: false}}, false},
	}
	for _, c := range cases {
		if got := c.r.ready(); got != c.want {
			t.Errorf("%s: ready() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFirstErr(t *testing.T) {
	if err := firstErr(nil, nil, nil); err != nil {
		t.Errorf("got %v, want nil", err)
	}
	want := errBoom
	if got := firstErr(nil, want, nil); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }
