package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultProxyBase = "https://proxy.golang.org"
	defaultSumBase   = "https://sum.golang.org"
)

// endpoints holds the base URLs so tests can point at an httptest server.
type endpoints struct {
	proxyBase string
	sumBase   string
	client    *http.Client
}

func defaultEndpoints() endpoints {
	return endpoints{
		proxyBase: defaultProxyBase,
		sumBase:   defaultSumBase,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

// probeResult is the outcome of one HTTP GET against the proxy or sumdb.
type probeResult struct {
	ok         bool
	statusCode int
	body       string
	err        error
}

func (e endpoints) get(url string) probeResult {
	resp, err := e.client.Get(url)
	if err != nil {
		return probeResult{err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return probeResult{
		ok:         resp.StatusCode == http.StatusOK,
		statusCode: resp.StatusCode,
		body:       string(body),
	}
}

// report is every probe made against one module@version, plus the raw list
// of published versions (for the "was this version ever released" check).
type report struct {
	module, version string
	latest          probeResult
	list            probeResult
	versionInfo     probeResult
	sum             probeResult
}

func (e endpoints) probe(module, version string) report {
	mod := escapePath(module)
	ver := escapePath(version)
	return report{
		module:      module,
		version:     version,
		latest:      e.get(fmt.Sprintf("%s/%s/@latest", e.proxyBase, mod)),
		list:        e.get(fmt.Sprintf("%s/%s/@v/list", e.proxyBase, mod)),
		versionInfo: e.get(fmt.Sprintf("%s/%s/@v/%s.info", e.proxyBase, mod, ver)),
		sum:         e.get(fmt.Sprintf("%s/lookup/%s@%s", e.sumBase, mod, ver)),
	}
}

// moduleKnown reports whether the proxy has heard of the module at all,
// independent of the specific version being checked.
func (r report) moduleKnown() bool {
	if r.latest.ok {
		return true
	}
	// @v/list returns 200 with an empty body for a module with zero
	// tagged releases, so any 200 counts even if list.body is empty.
	return r.list.ok
}

func (r report) listedVersions() []string {
	if !r.list.ok || strings.TrimSpace(r.list.body) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(r.list.body), "\n")
}

func (r report) ready() bool {
	return r.versionInfo.ok && r.sum.ok
}

// versionInfoBody mirrors the JSON the proxy returns from an @v/<version>.info
// or @latest request. Only used to surface the resolved version in output.
type versionInfoBody struct {
	Version string
	Time    string
}

func parseVersionInfo(body string) (versionInfoBody, error) {
	var v versionInfoBody
	err := json.Unmarshal([]byte(body), &v)
	return v, err
}
