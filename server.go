package main

// The window talks to the app over HTTP on the loopback interface, and the app pushes changes to the window over a
// Server-Sent Events stream, so the window reacts at once instead of showing a stale snapshot.
//
// Protections: loopback only; every request needs the random per-run token (set as a cookie when the window first
// opens) and a loopback Host header; cross-site POSTs are refused because they need a JSON content type and a
// matching Origin.

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var uiFS embed.FS

// uiDir, when set (developer flag --ui-dir), serves the window's files from disk so a design change needs no rebuild.
var uiDir string

func readUI(name string) ([]byte, error) {
	if uiDir != "" {
		return os.ReadFile(filepath.Join(uiDir, strings.TrimPrefix(name, "ui/")))
	}
	return uiFS.ReadFile(name)
}

// ---------------------------------------------------------------- event hub

type Hub struct {
	mu         sync.Mutex
	clients    map[chan []byte]struct{}
	connected  bool      // somebody has connected at least once
	emptySince time.Time // when the last client left
}

func newHub() *Hub { return &Hub{clients: map[chan []byte]struct{}{}} }

func (h *Hub) subscribe() chan []byte {
	ch := make(chan []byte, 256)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.connected = true
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	if len(h.clients) == 0 {
		h.emptySince = time.Now()
	}
	h.mu.Unlock()
}

func (h *Hub) wasConnected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// idleFor is how long nobody has been watching, once somebody has.
func (h *Hub) idleFor() (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.connected || len(h.clients) > 0 {
		return 0, false
	}
	return time.Since(h.emptySince), true
}

// Broadcast never blocks: a window that is too slow to read simply misses an update, and every event is a
// "this changed, fetch it" hint or a complete snapshot, so the next one repairs it.
func (h *Hub) Broadcast(event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte("event: " + event + "\ndata: " + string(b) + "\n\n")
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	h.mu.Unlock()
}

// ---------------------------------------------------------------- server

type Server struct {
	app   *App
	mux   *http.ServeMux
	hosts map[string]bool
	quit  func()
}

func newServer(app *App, port int, quit func()) *Server {
	s := &Server{app: app, mux: http.NewServeMux(), quit: quit, hosts: map[string]bool{}}
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		s.hosts[h+":"+strconv.Itoa(port)] = true
	}
	m := s.mux
	m.HandleFunc("GET /{$}", s.index)
	m.HandleFunc("GET /app.js", s.static("ui/app.js", "text/javascript; charset=utf-8"))
	m.HandleFunc("GET /app.css", s.static("ui/app.css", "text/css; charset=utf-8"))
	m.HandleFunc("GET /icon.svg", s.static("ui/icon.svg", "image/svg+xml"))
	m.HandleFunc("GET /events", s.events)
	m.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, app.State()) })
	m.HandleFunc("GET /api/tree", s.tree)
	m.HandleFunc("GET /api/groups", s.groups)
	m.HandleFunc("GET /api/errors", func(w http.ResponseWriter, r *http.Request) {
		app.smu.Lock()
		errs := append([]ScanError{}, app.errs...)
		app.smu.Unlock()
		writeJSON(w, 200, errs)
	})
	m.HandleFunc("GET /api/categories", func(w http.ResponseWriter, r *http.Request) {
		cats, rev := app.Categories()
		writeJSON(w, 200, map[string]any{"cats": cats, "rev": rev, "defaults": defaultCategories()})
	})
	m.HandleFunc("POST /api/categories", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var cats []Category
		if err := json.Unmarshal(body, &cats); err != nil {
			return nil, 400, err
		}
		if err := app.SetCategories(cats); err != nil {
			return nil, 400, err
		}
		got, rev := app.Categories()
		return map[string]any{"cats": got, "rev": rev}, 200, nil
	}))
	m.HandleFunc("POST /api/categories/reset", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		if err := app.SetCategories(defaultCategories()); err != nil {
			return nil, 500, err
		}
		got, rev := app.Categories()
		return map[string]any{"cats": got, "rev": rev}, 200, nil
	}))
	m.HandleFunc("POST /api/scan", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var cfg Settings
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, 400, err
		}
		if err := app.StartScan(cfg); err != nil {
			code := 400
			if err == errBusy {
				code = 409
			}
			return nil, code, err
		}
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/cancel", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		app.Cancel()
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/delete", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Paths      []string `json:"paths"`
			Mode       string   `json:"mode"`
			AllowAll   bool     `json:"allowAll"`
			SkipVerify bool     `json:"skipVerify"` // the user chose speed over the byte-for-byte check
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		del := app.Delete
		if req.SkipVerify {
			del = app.DeleteUnchecked
		}
		res, err := del(req.Paths, req.Mode, req.AllowAll)
		if err != nil {
			return nil, 400, err
		}
		return map[string]any{"results": res}, 200, nil
	}))
	m.HandleFunc("POST /api/trash", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Path == "" {
			return nil, 400, fmt.Errorf("a path is needed")
		}
		return app.TrashPath(req.Path), 200, nil
	}))
	m.HandleFunc("POST /api/reveal", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		if err := app.Reveal(req.Path); err != nil {
			return nil, 400, err
		}
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/empty-bin", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Drive string `json:"drive"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		freed, err := app.EmptyRecycleBin(req.Drive)
		if err != nil {
			return nil, 400, err
		}
		return map[string]any{"bytes": freed}, 200, nil
	}))
	m.HandleFunc("POST /api/dirfiles", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Dirs []string `json:"dirs"`
			List bool     `json:"list"`
		}
		if err := json.Unmarshal(body, &req); err != nil || len(req.Dirs) == 0 || len(req.Dirs) > 200 {
			return nil, 400, fmt.Errorf("between 1 and 200 folders are needed")
		}
		return map[string]any{"dirs": app.DirFiles(req.Dirs, req.List)}, 200, nil
	}))
	// zips and the folders they were unpacked into (archives.go)
	m.HandleFunc("POST /api/archives", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		list, known := app.Archives()
		return map[string]any{"known": known, "archives": list}, 200, nil
	}))
	archiveReq := func(body json.RawMessage) (zip, base string, strip bool, which string, err error) {
		var req struct {
			Zip   string `json:"zip"`
			Base  string `json:"base"`
			Strip bool   `json:"strip"`
			Which string `json:"which"`
		}
		if err = json.Unmarshal(body, &req); err != nil {
			return
		}
		if req.Zip == "" || req.Base == "" || !app.inScannedRoots(req.Zip) || !app.inScannedRoots(req.Base) {
			err = fmt.Errorf("that zip and folder are not part of the scan")
		}
		return req.Zip, req.Base, req.Strip, req.Which, err
	}
	// progress of a long comparison, pushed to the window a few times a second
	archiveProgress := func(zipPath string) func(done, total int64) {
		var last time.Time
		return func(done, total int64) {
			if time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				app.hub.Broadcast("archive", map[string]any{"zip": zipPath, "done": done, "total": total})
			}
		}
	}
	m.HandleFunc("POST /api/archive/check", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		zipPath, base, strip, _, err := archiveReq(body)
		if err != nil {
			return nil, 400, err
		}
		chk, err := app.CheckArchive(r.Context(), zipPath, base, strip, archiveProgress(zipPath))
		if err != nil {
			return nil, 400, err
		}
		return chk, 200, nil
	}))
	m.HandleFunc("POST /api/archive/trash", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		zipPath, base, strip, which, err := archiveReq(body)
		if err != nil {
			return nil, 400, err
		}
		res, chk, err := app.TrashArchivePart(r.Context(), zipPath, base, strip, which, archiveProgress(zipPath))
		if err != nil {
			return nil, 400, err
		}
		return map[string]any{"result": res, "check": chk}, 200, nil
	}))
	m.HandleFunc("POST /api/archive/checkall", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Items []ArchiveItem `json:"items"`
		}
		if err := json.Unmarshal(body, &req); err != nil || len(req.Items) == 0 || len(req.Items) > 1000 {
			return nil, 400, fmt.Errorf("between 1 and 1000 zips are needed")
		}
		for _, it := range req.Items {
			if !app.inScannedRoots(it.Zip) || !app.inScannedRoots(it.Base) {
				return nil, 400, fmt.Errorf("that zip and folder are not part of the scan")
			}
		}
		if err := app.CompareAll(req.Items); err != nil {
			return nil, 409, err
		}
		return map[string]any{"started": len(req.Items)}, 200, nil
	}))
	m.HandleFunc("POST /api/archive/cancel", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		app.CancelCompare()
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/live", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			On   bool   `json:"on"`
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		if err := app.SetLive(req.On, req.Path); err != nil {
			return nil, 400, err
		}
		return map[string]any{"on": req.On}, 200, nil
	}))
	m.HandleFunc("POST /api/verify", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		return map[string]any{"gone": app.Verify()}, 200, nil
	}))
	m.HandleFunc("POST /api/rescan", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		delta, err := app.Rescan(req.Path)
		if err != nil {
			return nil, 400, err
		}
		return map[string]any{"delta": delta}, 200, nil
	}))
	m.HandleFunc("POST /api/import", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Path) == "" {
			return nil, 400, fmt.Errorf("give the path of a duplicates.json file or its folder")
		}
		n, err := app.Import(strings.Trim(strings.TrimSpace(req.Path), `"`))
		if err != nil {
			return nil, 400, err
		}
		return map[string]any{"groups": n}, 200, nil
	}))
	m.HandleFunc("GET /api/reports", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, discoverReports(app.reportBases()))
	})
	m.HandleFunc("POST /api/prefs", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var in map[string]any
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, 400, err
		}
		app.mu.Lock()
		if app.settings.Prefs == nil {
			app.settings.Prefs = map[string]any{}
		}
		for k, v := range in {
			switch v.(type) {
			case bool, string, float64:
				app.settings.Prefs[k] = v // only simple values: this is not a place to stash anything else
			}
		}
		app.mu.Unlock()
		app.saveSettings()
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /mcp", s.mcp)
	m.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
	})
	m.HandleFunc("POST /api/approval", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			ID      string `json:"id"`
			Approve bool   `json:"approve"`
			Trust   bool   `json:"trust"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		if !app.ResolveApproval(req.ID, req.Approve, req.Trust) {
			return nil, 404, fmt.Errorf("that request is no longer waiting")
		}
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/view", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var v ViewState
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, 400, err
		}
		app.SetView(v)
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/suggestions/ack", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		app.AckSuggestions()
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("GET /api/claude", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, claudeInfo()) })
	m.HandleFunc("POST /api/claude/connect", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct{ Target, Path string }
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		var out string
		var err error
		switch req.Target {
		case "desktop":
			err = connectDesktop(req.Path)
		case "cli":
			out, err = connectCLI()
		default:
			err = fmt.Errorf("target must be desktop or cli")
		}
		if err != nil {
			msg := err.Error()
			if out != "" {
				msg += ": " + out
			}
			return nil, 400, fmt.Errorf("%s", msg)
		}
		return map[string]any{"info": claudeInfo(), "output": out}, 200, nil
	}))
	m.HandleFunc("POST /api/claude/disconnect", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct{ Target, Path string }
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		var err error
		switch req.Target {
		case "desktop":
			err = disconnectDesktop(req.Path)
		case "cli":
			_, err = disconnectCLI()
		default:
			err = fmt.Errorf("target must be desktop or cli")
		}
		if err != nil {
			return nil, 400, err
		}
		return map[string]any{"info": claudeInfo()}, 200, nil
	}))
	m.HandleFunc("GET /api/chat/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"messages": app.chat.History(), "state": app.chat.state()})
	})
	m.HandleFunc("POST /api/chat/send", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct{ Text string }
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		if err := app.chat.Send(req.Text); err != nil {
			return nil, 400, err
		}
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/chat/stop", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		app.chat.Stop()
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/chat/reset", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		app.chat.Reset()
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/prefer", s.post(func(r *http.Request, body json.RawMessage) (any, int, error) {
		var req struct {
			Prefer []string `json:"prefer"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, 400, err
		}
		app.mu.Lock()
		app.settings.Prefer = req.Prefer
		app.mu.Unlock()
		app.saveSettings()
		return map[string]any{"ok": true}, 200, nil
	}))
	// The only requests that leave this computer, and only when the user presses the button (see update.go).
	m.HandleFunc("POST /api/update/check", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		info, err := checkForUpdate(r.Context(), version)
		if err != nil {
			return nil, 502, err
		}
		return info, 200, nil
	}))
	m.HandleFunc("POST /api/update/open", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		if err := openURL(releasesPage); err != nil { // always the same page: nothing in the request or in GitHub's reply picks it
			return nil, 500, fmt.Errorf("could not open the browser: %v", err)
		}
		return map[string]any{"ok": true}, 200, nil
	}))
	m.HandleFunc("POST /api/quit", s.post(func(r *http.Request, _ json.RawMessage) (any, int, error) {
		go func() { time.Sleep(100 * time.Millisecond); s.quit() }()
		return map[string]any{"ok": true}, 200, nil
	}))
	return s
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		if !s.hosts[r.Host] {
			http.Error(w, "unexpected Host header", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || !s.hosts[u.Host] {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Get("t") != "" {
			if !tokenOK(r.URL.Query().Get("t"), s.app.token) {
				http.Error(w, "wrong token", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "ds", Value: s.app.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		ok := false
		if c, err := r.Cookie("ds"); err == nil && tokenOK(c.Value, s.app.token) {
			ok = true
		}
		if tokenOK(r.Header.Get("X-Token"), s.app.token) {
			ok = true
		}
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && tokenOK(h[7:], s.app.token) {
			ok = true // MCP clients that can only send an Authorization header
		}
		if !ok {
			http.Error(w, "Open DiskScout from its own window: this page needs the token the app gives it.", http.StatusForbidden)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

func tokenOK(got, want string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// post wraps a mutating endpoint: JSON content type required, body size capped, errors reported as {"error": ...}.
func (s *Server) post(f func(r *http.Request, body json.RawMessage) (any, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "expected application/json"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON: " + err.Error()})
			return
		}
		v, code, err := f(r, body)
		if err != nil {
			writeJSON(w, code, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, code, v)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	b, err := readUI("ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (s *Server) static(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := readUI(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	ch := s.app.hub.subscribe()
	defer s.app.hub.unsubscribe(ch)
	st, _ := json.Marshal(s.app.State())
	fmt.Fprintf(w, "retry: 1000\nevent: hello\ndata: %s\n\n", st)
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			w.Write(msg)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	depth, _ := strconv.Atoi(q.Get("depth"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if depth < 1 || depth > 4 {
		depth = 3
	}
	if limit < 5 || limit > 300 {
		limit = 60
	}
	s.app.mu.RLock()
	t := s.app.tree
	s.app.mu.RUnlock()
	if t == nil {
		writeJSON(w, 404, map[string]string{"error": "no scan yet"})
		return
	}
	filter, bad := parseFilter(q.Get("filter"))
	it, crumbs, ok := t.View(q.Get("path"), ViewOpts{Depth: depth, Limit: limit, HideSys: q.Get("hideSys") == "1",
		ShowFree: q.Get("free") != "0", ShowUnacc: q.Get("unacc") != "0", Filter: filter, Dup: s.app.isDup})
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "that folder is not on the map"})
		return
	}
	writeJSON(w, 200, map[string]any{"node": it, "crumbs": crumbs, "rev": s.app.treeRev.Load(), "complete": t.Complete, "roots": t.Roots, "filterBad": bad})
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	epoch, _ := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
	writeJSON(w, 200, s.app.GroupsSince(since, epoch))
}
