package main

import "testing"

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
