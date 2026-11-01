package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testServer struct {
	*httptest.Server
	app  *App
	quit chan struct{}
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	app := newTestApp(t)
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	quit := make(chan struct{}, 1)
	srv := newServer(app, port, func() { quit <- struct{}{} })
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return &testServer{ts, app, quit}
}

func (s *testServer) do(t *testing.T, method, path string, body any, mutate ...func(*http.Request)) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.URL+path, rd)
	req.Header.Set("X-Token", "tok")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mutate {
		m(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func TestServerProtections(t *testing.T) {
	s := newTestServer(t)
	check := func(name string, want int, method, path string, body any, mutate ...func(*http.Request)) {
		t.Helper()
		if resp, out := s.do(t, method, path, body, mutate...); resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d (%s)", name, resp.StatusCode, want, out)
		}
	}
	check("with the token", 200, "GET", "/api/state", nil)
	check("no token", 403, "GET", "/api/state", nil, func(r *http.Request) { r.Header.Del("X-Token") })
	check("wrong token", 403, "GET", "/api/state", nil, func(r *http.Request) { r.Header.Set("X-Token", "nope") })
	check("foreign Host header (DNS rebinding)", 403, "GET", "/api/state", nil, func(r *http.Request) { r.Host = "evil.example:80" })
	check("foreign Origin", 403, "POST", "/api/cancel", map[string]any{}, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	check("own Origin is fine", 200, "POST", "/api/cancel", map[string]any{}, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	check("a form post is not JSON", 415, "POST", "/api/cancel", map[string]any{}, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	check("broken JSON", 400, "POST", "/api/scan", nil, func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Body = io.NopCloser(strings.NewReader("{oops"))
	})
	check("unknown path", 404, "GET", "/api/nothing", nil)

	// the first visit carries the token in the URL and swaps it for a cookie
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.Get(s.URL + "/?t=tok")
	if resp.StatusCode != 302 || len(resp.Cookies()) == 0 || !resp.Cookies()[0].HttpOnly {
		t.Fatalf("token visit: %d %v", resp.StatusCode, resp.Cookies())
	}
	req, _ := http.NewRequest("GET", s.URL+"/", nil)
	req.AddCookie(resp.Cookies()[0])
	page, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != 200 || !bytes.Contains(body, []byte("stub")) && len(body) == 0 {
		t.Fatalf("page with cookie: %d", page.StatusCode)
	}
	if resp, _ := client.Get(s.URL + "/?t=wrong"); resp.StatusCode != 403 {
		t.Fatalf("a wrong token in the URL gave %d", resp.StatusCode)
	}
	if c := page.Header.Get("Content-Security-Policy"); !strings.Contains(c, "default-src 'self'") {
		t.Fatalf("no CSP: %q", c)
	}
}

// sseReader yields (event, data) pairs from an event stream.
func sseReader(t *testing.T, s *testServer) (<-chan [2]string, func()) {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL+"/events", nil)
	req.Header.Set("X-Token", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan [2]string, 1024)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = line[7:]
			case strings.HasPrefix(line, "data: "):
				ch <- [2]string{ev, line[6:]}
			}
		}
	}()
	return ch, func() { resp.Body.Close() }
}

func waitEvent(t *testing.T, ch <-chan [2]string, name string, ok func(data string) bool) string {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case e, open := <-ch:
			if !open {
				t.Fatalf("stream closed while waiting for %q", name)
			}
			if e[0] == name && (ok == nil || ok(e[1])) {
				return e[1]
			}
		case <-timeout:
			t.Fatalf("no %q event", name)
		}
	}
}

func TestLiveEventsAndAPI(t *testing.T) {
	s := newTestServer(t)
	events, closeStream := sseReader(t, s)
	defer closeStream()

	hello := waitEvent(t, events, "hello", nil)
	var st StateResp
	if err := json.Unmarshal([]byte(hello), &st); err != nil || st.Status.Phase != "idle" || len(st.Cats) != len(defaultCategories()) {
		t.Fatalf("hello: %v %s", err, hello)
	}
	if s.app.hub.count() != 1 {
		t.Fatalf("hub sees %d clients", s.app.hub.count())
	}

	root, c := fixture(t)
	if resp, out := s.do(t, "POST", "/api/scan", Settings{Roots: []string{root}, FindDupes: true, MinDup: 1, Workers: 2}); resp.StatusCode != 200 {
		t.Fatalf("scan: %d %s", resp.StatusCode, out)
	}
	if resp, _ := s.do(t, "POST", "/api/scan", Settings{Roots: []string{root}}); resp.StatusCode != 409 && resp.StatusCode != 200 {
		t.Fatalf("a second scan while running should be 409 (or 200 if the first already finished), got %d", resp.StatusCode)
	}
	// the window is told what happens, as it happens
	waitEvent(t, events, "status", func(d string) bool {
		return strings.Contains(d, `"phase":"scanning"`) || strings.Contains(d, `"phase":"hashing"`)
	})
	waitEvent(t, events, "groups", func(d string) bool { return strings.Contains(d, `"count":1`) })
	waitEvent(t, events, "status", func(d string) bool { return strings.Contains(d, `"phase":"done"`) })
	waitDone(t, s.app)

	// the map
	_, out := s.do(t, "GET", "/api/tree?path="+root, nil)
	var tree struct {
		Node struct {
			Size     int64
			Children []Item
		}
		Crumbs   []Crumb
		Complete bool
	}
	json.Unmarshal(out, &tree)
	if tree.Node.Size != 4*400*1024 || len(tree.Node.Children) == 0 || !tree.Complete || tree.Crumbs[0].Name != "This PC" {
		t.Fatalf("tree: %s", out)
	}
	if resp, _ := s.do(t, "GET", "/api/tree?path="+filepath.Join(root, "nope"), nil); resp.StatusCode != 404 {
		t.Fatalf("an unknown folder should be 404, got %d", resp.StatusCode)
	}
	_, out = s.do(t, "GET", "/api/tree", nil) // the top: every scan root
	if !strings.Contains(string(out), `"This PC"`) {
		t.Fatalf("root view: %s", out)
	}

	// what the folder view asks: how many files the duplicate search saw in a folder
	_, out = s.do(t, "POST", "/api/dirfiles", map[string]any{"dirs": []string{filepath.Join(root, "dupe")}, "list": true})
	if !strings.Contains(string(out), `"known":true`) || !strings.Contains(string(out), `"files":2`) || !strings.Contains(string(out), `copy1.bin`) {
		t.Fatalf("dirfiles: %s", out)
	}
	if resp, _ := s.do(t, "POST", "/api/dirfiles", map[string]any{"dirs": []string{}}); resp.StatusCode != 400 {
		t.Fatalf("an empty folder list should be 400, got %d", resp.StatusCode)
	}

	// deleting through the API pushes the change to the window, after comparing the copy with the one that stays
	_, out = s.do(t, "POST", "/api/delete", map[string]any{"paths": []string{c[2]}, "mode": "permanent"})
	if !strings.Contains(string(out), `"ok":true`) || !strings.Contains(string(out), `"verified"`) {
		t.Fatalf("delete: %s", out)
	}
	waitEvent(t, events, "deleted", func(d string) bool { return strings.Contains(d, `"count":1`) })
	_, out = s.do(t, "GET", "/api/groups?since=0&epoch=0", nil)
	var gr GroupsResp
	json.Unmarshal(out, &gr)
	if len(gr.Groups) != 1 || countGone(gr.Groups[0]) != 1 {
		t.Fatalf("groups after delete: %s", out)
	}
	if resp, _ := s.do(t, "POST", "/api/delete", map[string]any{"paths": []string{c[1]}, "mode": "bogus"}); resp.StatusCode != 400 {
		t.Fatalf("a bogus mode should be 400, got %d", resp.StatusCode)
	}

	// categories: edit, invalid edit, reset
	cats := []Category{{Name: "Everything", Risk: "low", Rules: nil}}
	if resp, out := s.do(t, "POST", "/api/categories", cats); resp.StatusCode != 200 {
		t.Fatalf("save categories: %d %s", resp.StatusCode, out)
	}
	waitEvent(t, events, "categories", nil)
	if resp, _ := s.do(t, "POST", "/api/categories", []Category{{Name: "x", Rules: []string{"re:("}}}); resp.StatusCode != 400 {
		t.Fatal("an invalid rule should be 400")
	}
	_, out = s.do(t, "POST", "/api/categories/reset", map[string]any{})
	var reset struct{ Cats []Category }
	json.Unmarshal(out, &reset)
	if len(reset.Cats) != len(defaultCategories()) {
		t.Fatalf("reset: %s", out)
	}

	// a deletion made outside the app is noticed
	removeFile(t, c[1])
	_, out = s.do(t, "POST", "/api/verify", map[string]any{})
	if !strings.Contains(string(out), `"gone":1`) {
		t.Fatalf("verify: %s", out)
	}

	// errors list and quitting
	if resp, _ := s.do(t, "GET", "/api/errors", nil); resp.StatusCode != 200 {
		t.Fatal("errors")
	}
	s.do(t, "POST", "/api/quit", map[string]any{})
	select {
	case <-s.quit:
	case <-time.After(3 * time.Second):
		t.Fatal("quit was not called")
	}
}

func TestHubNeverBlocks(t *testing.T) {
	h := newHub()
	ch := h.subscribe()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ { // nobody reads: a slow window must not stall the scan
			h.Broadcast("status", map[string]int{"i": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast blocked on a slow client")
	}
	if d, ok := h.idleFor(); ok {
		t.Fatalf("idle while a client is connected: %v", d)
	}
	h.unsubscribe(ch)
	time.Sleep(20 * time.Millisecond)
	if d, ok := h.idleFor(); !ok || d <= 0 {
		t.Fatal("idleFor should report time since the last client left")
	}
}
