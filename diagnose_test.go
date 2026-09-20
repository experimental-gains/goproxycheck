package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeProxy serves a scripted set of responses keyed by exact path, so each
// test can simulate one specific proxy/sumdb state without hitting the network.
func fakeProxy(t *testing.T, routes map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, ok := routes[r.URL.Path]
		if !ok {
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
		w.WriteHeader(code)
		if code == http.StatusOK {
			fmt.Fprint(w, `{"Version":"v0.1.0","Time":"2026-09-19T00:00:00Z"}`)
		}
	}))
	return srv
}

func TestDiagnose_Ready(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusReady {
		t.Fatalf("status = %s, want ready; message: %s", got.status, got.message)
	}
}

func TestDiagnose_NegativeCache(t *testing.T) {
	// @latest/@v/list know about the module and list this exact version,
	// but the per-version .info 404s — the poisoned-cache signature.
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	// @v/list needs a real body listing the version for the "known but
	// poisoned" branch to trigger; fake it via a second server variant.
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"Version":"v0.1.0"}`)
		case "/example.com/mod/@v/list":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "v0.1.0\n")
		case "/example.com/mod/@v/v0.1.0.info":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer listSrv.Close()
	ep.proxyBase = listSrv.URL

	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusNegativeCache {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNegativeCache, got.message)
	}
}

func TestDiagnose_ModuleUnknown(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/nope/@latest":        http.StatusNotFound,
		"/example.com/nope/@v/list":        http.StatusNotFound,
		"/example.com/nope/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/nope@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/nope", "v0.1.0")
	got := diagnose(r)
	if got.status != statusModuleUnknown {
		t.Fatalf("status = %s, want %s", got.status, statusModuleUnknown)
	}
}

func TestDiagnose_SumdbLag(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusSumdbLag {
		t.Fatalf("status = %s, want %s", got.status, statusSumdbLag)
	}
}

func TestDiagnose_NetworkError(t *testing.T) {
	r := report{
		module:  "example.com/mod",
		version: "v0.1.0",
		latest:  probeResult{err: fmt.Errorf("dial tcp: connection refused")},
	}
	got := diagnose(r)
	if got.status != statusNetworkError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNetworkError, got.message)
	}
}

func TestDefaultEndpoints(t *testing.T) {
	e := defaultEndpoints()
	if e.proxyBase != defaultProxyBase || e.sumBase != defaultSumBase || e.client == nil {
		t.Errorf("got %+v", e)
	}
}

func TestDiagnose_NotYetIndexed(t *testing.T) {
	// Module known (has other versions) but this version isn't in @v/list
	// at all yet — ordinary indexing lag, distinct from negative-cache.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"Version":"v0.0.9"}`)
		case "/example.com/mod/@v/list":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "v0.0.9\n")
		case "/example.com/mod/@v/v0.1.0.info":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusNotYetIndexed {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNotYetIndexed, got.message)
	}
}
