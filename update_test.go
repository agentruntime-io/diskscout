package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub stands in for GitHub: it records what it was asked and answers with a fixed status and body.
type fakeGitHub struct {
	*httptest.Server
	mu     sync.Mutex
	hits   int
	method string
	header http.Header
}

func newFakeGitHub(t *testing.T, status int, body string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.method, f.header = r.Method, r.Header.Clone()
		f.mu.Unlock()
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	old := updateAPI
	updateAPI = f.URL
	t.Cleanup(func() { updateAPI = old })
	return f
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func TestVersionCompare(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		newer           bool
	}{
		{"v1.8.0", "1.7.1", true},
		{"1.7.1", "1.7.1", false},
		{"v1.7.0", "1.7.1", false}, // a build newer than the newest release is not "out of date"
		{"v1.10.0", "1.9.9", true}, // numbers, not text: 10 is more than 9
		{"v2.0.0", "1.99.99", true},
		{"v1.7.2", "1.7.1", true},
	} {
		l, ok1 := parseVersion(c.latest)
		cur, ok2 := parseVersion(c.current)
		if !ok1 || !ok2 {
			t.Fatalf("%q / %q should both parse", c.latest, c.current)
		}
		if got := versionNewer(l, cur); got != c.newer {
			t.Errorf("latest %s vs current %s: newer = %v, want %v", c.latest, c.current, got, c.newer)
		}
	}
}

func TestParseVersionRefusesWhatIsNotAVersion(t *testing.T) {
	for _, s := range []string{"", "latest", "nightly", "v1.8", "1.8.0-rc1", "v1.8.0/../../x", "1.2.3.4", "v1.8.0\n", " 1.8.0", "v99999.0.0", "v1.8.0 "} {
		if _, ok := parseVersion(s); ok {
			t.Errorf("%q must not parse as a version", s)
		}
	}
}

func TestCheckForUpdateReportsTheNewestRelease(t *testing.T) {
	// the reply carries a link too: it must be ignored, never followed or shown
	f := newFakeGitHub(t, 200, `{"tag_name":"v1.8.0","html_url":"https://evil.example/download","body":"run this"}`)
	info, err := checkForUpdate(t.Context(), "1.7.1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Latest != "1.8.0" || info.Current != "1.7.1" || !info.Newer {
		t.Errorf("got %+v", info)
	}
	b, _ := json.Marshal(info)
	if strings.Contains(string(b), "evil") {
		t.Errorf("a link from GitHub's reply leaked into the answer: %s", b)
	}

	// what leaves the computer: one GET, the version in the User-Agent, no credentials, no cookies, nothing about the machine
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits != 1 || f.method != "GET" {
		t.Errorf("want exactly one GET, got %d %s", f.hits, f.method)
	}
	if ua := f.header.Get("User-Agent"); ua != "DiskScout/1.7.1" {
		t.Errorf("User-Agent = %q", ua)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Token", "Referer"} {
		if f.header.Get(h) != "" {
			t.Errorf("the request carries %s", h)
		}
	}
}

func TestCheckForUpdateWhenUpToDate(t *testing.T) {
	newFakeGitHub(t, 200, `{"tag_name":"v1.7.1"}`)
	info, err := checkForUpdate(t.Context(), "1.7.1")
	if err != nil || info.Newer || info.Latest != "1.7.1" {
		t.Errorf("same version: %+v, %v", info, err)
	}
	updateAPI = newFakeGitHub(t, 200, `{"tag_name":"v1.7.0"}`).URL
	info, err = checkForUpdate(t.Context(), "1.7.1")
	if err != nil || info.Newer {
		t.Errorf("an older release must not look like an update: %+v, %v", info, err)
	}
}

func TestCheckForUpdateSaysWhatWentWrong(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"no release yet", 404, `{}`, "no release"},
		{"rate limited (403)", 403, `{}`, "limiting"},
		{"rate limited (429)", 429, `{}`, "limiting"},
		{"server error", 500, `oops`, "error (500)"},
		{"not JSON", 200, `<html>`, "not understood"},
		{"a name that is not a version", 200, `{"tag_name":"nightly"}`, "version number"},
		{"a hostile name", 200, `{"tag_name":"v1.8.0/../../x"}`, "version number"},
	} {
		newFakeGitHub(t, c.status, c.body)
		_, err := checkForUpdate(t.Context(), "1.7.1")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want it to say %q", c.name, err, c.want)
		}
	}

	old := updateAPI
	defer func() { updateAPI = old }()
	updateAPI = "http://127.0.0.1:1/" // nothing listens there
	if _, err := checkForUpdate(t.Context(), "1.7.1"); err == nil || !strings.Contains(err.Error(), "could not reach GitHub") {
		t.Errorf("offline: error = %v", err)
	}
	if _, err := checkForUpdate(t.Context(), "dev"); err == nil {
		t.Error("a build with no version number cannot be compared and must say so")
	}
}

func TestCheckForUpdateGivesUpWhenGitHubIsSlow(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(slow.Close)
	oldAPI, oldTimeout := updateAPI, updateTimeout
	updateAPI, updateTimeout = slow.URL, 150*time.Millisecond
	defer func() { updateAPI, updateTimeout = oldAPI, oldTimeout }()

	_, err := checkForUpdate(t.Context(), "1.7.1")
	if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Errorf("a slow GitHub must be reported as slow, not as being offline: %v", err)
	}
}

func TestUpdateEndpoints(t *testing.T) {
	f := newFakeGitHub(t, 200, `{"tag_name":"v99.0.0"}`)
	s := newTestServer(t)

	resp, out := s.do(t, "POST", "/api/update/check", map[string]any{})
	var info UpdateInfo
	if resp.StatusCode != 200 || json.Unmarshal(out, &info) != nil || !info.Newer || info.Latest != "99.0.0" || info.Current != version {
		t.Fatalf("check: %d %s", resp.StatusCode, out)
	}
	if resp, _ := s.do(t, "POST", "/api/update/check", map[string]any{}, func(r *http.Request) { r.Header.Del("X-Token") }); resp.StatusCode != 403 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := s.do(t, "POST", "/api/update/check", map[string]any{}, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }); resp.StatusCode != 403 {
		t.Errorf("foreign Origin: %d", resp.StatusCode)
	}
	if resp, _ := s.do(t, "GET", "/api/update/check", nil); resp.StatusCode != 405 {
		t.Errorf("a GET must not trigger a check: %d", resp.StatusCode)
	}
	if f.count() != 1 {
		t.Errorf("only the one real request should have reached GitHub, got %d", f.count())
	}

	// a failure is a readable message with a non-200 status
	newFakeGitHub(t, 404, `{}`)
	resp, out = s.do(t, "POST", "/api/update/check", map[string]any{})
	if resp.StatusCode != 502 || !strings.Contains(string(out), "no release") {
		t.Errorf("failure: %d %s", resp.StatusCode, out)
	}
}

func TestUpdateOpenAlwaysOpensTheFixedPage(t *testing.T) {
	var opened []string
	old := openURL
	openURL = func(u string) error { opened = append(opened, u); return nil }
	defer func() { openURL = old }()

	s := newTestServer(t)
	// the request cannot choose the address
	if resp, _ := s.do(t, "POST", "/api/update/open", map[string]any{"url": "https://evil.example/"}); resp.StatusCode != 200 {
		t.Fatalf("open: %d", resp.StatusCode)
	}
	if len(opened) != 1 || opened[0] != releasesPage {
		t.Errorf("opened %v, want only %s", opened, releasesPage)
	}
	if !strings.HasPrefix(releasesPage, "https://github.com/agentruntime-io/diskscout/") {
		t.Errorf("the page is not this project's: %s", releasesPage)
	}
}

func TestNothingChecksForUpdatesByItself(t *testing.T) {
	f := newFakeGitHub(t, 200, `{"tag_name":"v99.0.0"}`)
	s := newTestServer(t)
	for _, p := range []string{"/api/state", "/api/errors", "/api/categories", "/"} {
		s.do(t, "GET", p, nil)
	}
	s.do(t, "POST", "/api/verify", map[string]any{})
	if n := f.count(); n != 0 {
		t.Errorf("GitHub was contacted %d time(s) without anyone pressing the button", n)
	}
}

// The README promises "no network connections except when you press Check for updates". Keep it true: any other file that
// starts to use the network has to be added here on purpose, and the README and user guide changed with it.
func TestOnlyKnownFilesUseTheNetwork(t *testing.T) {
	allowed := map[string]string{
		"update.go": "Check for updates, only when the button is pressed",
		"mcp.go":    "the stdio bridge talks to the local window on 127.0.0.1",
		"main.go":   "the local server listens on 127.0.0.1",
	}
	use := regexp.MustCompile(`http\.DefaultClient|http\.Client\{|http\.Get\(|http\.Post\(|http\.PostForm\(|http\.Head\(|net\.Dial|tls\.Dial|DialContext|net\.Listen|websocket`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := use.Find(src); m != nil {
			if _, ok := allowed[f]; !ok {
				t.Errorf("%s uses the network (%s). DiskScout promises no network connections except the update check: if this is intended, say so in the README and the user guide, then add the file here", f, m)
			}
		}
	}
}
