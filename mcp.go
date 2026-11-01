package main

// DiskScout as an MCP server, so Claude can use the app: look at the map and the duplicates, move the window around,
// start scans, suggest what to clean, and (only with the user's approval in the window) move things to the Recycle Bin.
//
// Two ways in, one set of tools:
//   - POST /mcp on the running app (used by the chat panel's own Claude Code process)
//   - `DiskScout.exe --mcp`, a stdio bridge that Claude Desktop and the Claude Code CLI start themselves; it starts the
//     DiskScout window if it is not running and forwards each tool call to it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type mcpTool struct {
	Name, Description string
	Schema            map[string]any
	Run               func(ctx context.Context, a *App, args map[string]any) (any, error)
}

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `DiskScout shows where a Windows disk's space went and finds duplicate files. Start with status, then look (folder, largest_files, duplicates) before advising. Use show to move the user's window to what you are discussing. Never remove anything on your own initiative: put proposals on the user's list with suggest_cleanup, and call trash only when the user has clearly asked you to remove specific things. trash only moves items to the Recycle Bin and the user must approve it in the DiskScout window; tell them to look there. Windows, Program Files and profile folders are off limits. To see inside things DiskScout cannot (Docker images and volumes, WSL, tool caches) use run_command: diagnostics such as docker system df -v run at once, and every cleanup or unlisted command waits for the user to approve the exact command line.`

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func tStr(d string) map[string]any  { return map[string]any{"type": "string", "description": d} }
func tNum(d string) map[string]any  { return map[string]any{"type": "number", "description": d} }
func tBool(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }
func tStrs(d string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": d}
}

func argStr(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func argNum(m map[string]any, k string, def float64) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return def
}
func argBool(m map[string]any, k string) bool { b, _ := m[k].(bool); return b }
func argStrs(m map[string]any, k string) []string {
	var out []string
	if arr, ok := m[k].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sized(n int64) map[string]any { return map[string]any{"bytes": n, "text": fmtBytes(n)} }

func mcpTools() []mcpTool {
	return []mcpTool{
		{"status", "Where things stand: the scan state, free space per drive, how many duplicate groups there are, what the user's window is showing, and any approval waiting. Call this first.",
			obj(map[string]any{}), toolStatus},
		{"scan", "Start scanning in the background (reads folders only; changes nothing). Returns at once; use wait_for_scan to wait. A map-only scan of a whole drive takes under a minute. Looking for duplicates also reads the contents of tens of GB, which takes minutes and works the disk hard, so leave find_duplicates off unless the user wants duplicates. With no roots it scans the drives chosen last time. It does not change the user's saved scan settings.",
			obj(map[string]any{"roots": tStrs("Folders or drives to scan, e.g. [\"C:\\\\\"]"), "find_duplicates": tBool("Also look for duplicate files by content (default false: just build the map)"), "min_duplicate_mb": tNum("Ignore files smaller than this when looking for duplicates (default 1)")}), toolScan},
		{"wait_for_scan", "Wait until the running scan finishes (or the timeout passes) and return its status.",
			obj(map[string]any{"timeout_seconds": tNum("How long to wait, up to 300 (default 60)")}), toolWait},
		{"folder", "List what is inside a folder on the space map, biggest first, with sizes and shares. With no path it lists the drives. At drive level it also reports free space and 'unaccounted' space (used according to Windows but not found by the scan: shadow copies, system areas). Items marked system are Windows-managed (Recycle Bin, pagefile, hibernation file).",
			obj(map[string]any{"path": tStr("Folder path; omit for the top"), "limit": tNum("How many items (default 30, max 100)"), "hide_system": tBool("Leave out system-managed items"), "filter": tStr("Optional filter: *.mov|*.mp4, >500MB, <30d (modified within), >1y, type:video, dup, -text to exclude")}), toolFolder},
		{"largest_files", "The biggest individual files (1 MB and up) under a path, optionally filtered. Good for finding what to look at.",
			obj(map[string]any{"path": tStr("Folder path; omit for everything scanned"), "count": tNum("How many (default 20, max 100)"), "filter": tStr("Same filter language as folder"), "hide_system": tBool("Leave out system-managed items")}), toolLargest},
		{"duplicates", "Groups of identical files (found by content), biggest reclaimable space first. Each copy says whether DiskScout suggests keeping it, and its category and risk (low = usually safe, medium = check first, high = leave alone, review = the user's own files).",
			obj(map[string]any{"category": tStr("Only groups with a removable copy in this category (name or part of it)"), "location": tStr("Only copies whose drive+folder contains this text"), "query": tStr("Only groups with a path containing this text"), "min_size_mb": tNum("Minimum file size"), "sort": tStr("reclaimable (default) or size"), "limit": tNum("Groups to return (default 20, max 100)"), "offset": tNum("Skip this many groups")}), toolDuplicates},
		{"compare_zip", "Check a .zip against the folder it was unpacked into, by reading both: for every file in the zip, is there a file at the matching path in the folder with exactly the same bytes, and is there anything in the folder that is not in the zip? Use it before saying that a zip and a folder are 'the same' or that one can be removed: do not guess from names or sizes. Read-only (it reads the contents, which takes a while for a big zip). Says plainly what it cannot tell: a zip it cannot open, entries it cannot read, a folder that shares its place with other files. RAR, 7z and tar archives cannot be opened.",
			obj(map[string]any{"path": tStr("The .zip file, as on the map")}, "path"), toolCompareZip},
		{"zips", "List the zip files that appear to have been unpacked into a folder (from the last scan that looked for duplicates, 1 MB and up), biggest first, with what has already been compared byte for byte, in the DiskScout window or by compare_zip. safe_to_remove means everything in the zip is in its folder, identical; not_the_same means the zip holds something the folder lacks or that changed; not_compared_yet means only names and sizes have been matched so far; cannot_compare means a RAR/7z/tar (not openable) or a zip that will not open. Use this to see the same list the user sees, then compare_zip on the ones that matter.",
			obj(map[string]any{"only": tStr("safe_to_remove, not_the_same, not_compared_yet or cannot_compare"), "query": tStr("Only zips whose path contains this text"), "limit": tNum("How many (default 25, max 100)")}), toolZips},
		{"categories", "The definitions DiskScout uses to say how risky a duplicate is to remove: each category's meaning, risk and rules, and how much duplicate space is in it right now.",
			obj(map[string]any{}), toolCategories},
		{"show", "Move the user's DiskScout window: switch tab, open a folder on the map, select an item. Use it to point at what you are talking about.",
			obj(map[string]any{"tab": tStr("space, dups or zips (the list of zips and their folders)"), "path": tStr("Folder to open on the map"), "select": tStr("File or folder to highlight")}), toolShow},
		{"suggest_cleanup", "Put items on the user's 'to clean' list with your reason. Nothing is removed; the user reviews the list and decides. Prefer this over trash.",
			obj(map[string]any{"items": map[string]any{"type": "array", "description": "What to suggest", "items": obj(map[string]any{"path": tStr("File or folder"), "reason": tStr("Why it is safe or worth removing, in one sentence")}, "path")}}, "items"), toolSuggest},
		{"run_command", "Run a command-line program and get its output, to look inside things DiskScout cannot see: Docker images, containers and volumes (docker system df -v), WSL disks (wsl -l -v), tool caches (go env GOCACHE, uv cache dir). Known read-only diagnostics run at once. Cleanup commands (docker system prune, go clean, npm cache clean, uv cache clean ...) and every other command wait for the user to approve the exact command line in the DiskScout window. There is no shell: give the program and its arguments separately, run ONE command per call, never chain with && or |. Prefer looking first, then proposing a specific cleanup and what it will free.",
			obj(map[string]any{"program": tStr("The program, such as docker, or a full path"), "args": tStrs("Its arguments, one per item"), "reason": tStr("Why you are running it, shown to the user if they must approve it"), "timeout_seconds": tNum("Time limit (default 60, up to 600; a prune can take minutes)")}, "program"), toolRun},
		{"trash", "Move items to the Recycle Bin, but only after the user clicks Approve in the DiskScout window (this call waits for that, up to 10 minutes). Call it only when the user has asked you to remove these specific things. kind=duplicates takes paths from a duplicates group (never removes the last copy); kind=paths takes files or folders from the map. The Recycle Bin frees no space until it is emptied.",
			obj(map[string]any{"kind": tStr("duplicates or paths"), "paths": tStrs("What to move to the Recycle Bin"), "reason": tStr("Shown to the user in the approval prompt")}, "kind", "paths"), toolTrash},
	}
}

// ---------------------------------------------------------------- the JSON-RPC layer

// mcpHandle answers one request. app may be nil (the stdio bridge answers the handshake itself).
func mcpHandle(ctx context.Context, app *App, req rpcReq) *rpcResp {
	reply := func(v any) *rpcResp { return &rpcResp{JSONRPC: "2.0", ID: req.ID, Result: v} }
	fail := func(code int, msg string) *rpcResp {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{code, msg}}
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		ver := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				ver = v
			}
		}
		return reply(map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": mcpName, "title": "DiskScout", "version": version},
			"instructions":    mcpInstructions,
		})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		var list []map[string]any
		for _, t := range mcpTools() {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
		}
		return reply(map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "bad params")
		}
		if app == nil {
			return fail(-32603, "not connected to DiskScout")
		}
		for _, t := range mcpTools() {
			if t.Name != p.Name {
				continue
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			out, err := t.Run(ctx, app, p.Arguments)
			if err != nil {
				return reply(map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": err.Error()}}})
			}
			text, ok := out.(string)
			if !ok {
				text = compactJSON(out)
			}
			return reply(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
		}
		return fail(-32602, "unknown tool "+p.Name)
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}
	return fail(-32601, "method not found: "+req.Method)
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	body = bytes.TrimSpace(body)
	var reqs []rpcReq
	batch := len(body) > 0 && body[0] == '['
	if batch {
		err = json.Unmarshal(body, &reqs)
	} else {
		var one rpcReq
		err = json.Unmarshal(body, &one)
		reqs = []rpcReq{one}
	}
	if err != nil {
		writeJSON(w, 400, &rpcResp{JSONRPC: "2.0", Error: &rpcErr{-32700, "parse error"}})
		return
	}
	if !batch && len(reqs) == 1 && reqs[0].Method == "tools/call" && len(reqs[0].ID) > 0 &&
		strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamCall(w, r, reqs[0])
		return
	}
	var resps []*rpcResp
	for _, q := range reqs {
		if resp := mcpHandle(r.Context(), s.app, q); resp != nil && len(q.ID) > 0 {
			resps = append(resps, resp)
		}
	}
	switch {
	case len(resps) == 0:
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, 200, resps)
	default:
		writeJSON(w, 200, resps[0])
	}
}

// mcpKeepalive is how often a streamed tool call says it is still working.
var mcpKeepalive = 15 * time.Second

// streamCall answers a tools/call as an event stream. A call can wait a long time (the user has to approve it in the
// window, a prune runs for minutes) and some clients drop a request that has not started to answer after a minute, which
// also cancels the pending approval. So the reply starts at once and goes on with progress notifications (or comments, when
// the client sent no progress token) until the answer is ready.
func (s *Server) streamCall(w http.ResponseWriter, r *http.Request, q rpcReq) {
	var p struct {
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	json.Unmarshal(q.Params, &p)
	fl, _ := w.(http.Flusher)
	send := func(text string) {
		io.WriteString(w, text)
		if fl != nil {
			fl.Flush()
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	send(": working\n\n")

	done := make(chan *rpcResp, 1)
	go func() { done <- mcpHandle(r.Context(), s.app, q) }()
	tick := time.NewTicker(mcpKeepalive)
	defer tick.Stop()
	for n := 1; ; n++ {
		select {
		case resp := <-done:
			b, _ := json.Marshal(resp)
			send("event: message\ndata: " + string(b) + "\n\n")
			return
		case <-tick.C:
			if len(p.Meta.ProgressToken) == 0 {
				send(": still working\n\n")
				continue
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{
				"progressToken": p.Meta.ProgressToken, "progress": n, "message": "Still working. If it is waiting for you, look at the DiskScout window."}})
			send("event: message\ndata: " + string(b) + "\n\n")
		}
	}
}

// ---------------------------------------------------------------- the tools

func briefStatus(a *App) map[string]any {
	st := a.Status()
	groups, reclaim := a.liveStats()
	scan := map[string]any{"phase": st.Phase, "source": st.Source, "files_seen": st.Files, "bytes_seen": sized(st.Bytes),
		"errors": st.Errors, "elapsed_seconds": int(st.Elapsed)}
	if st.Expected > 0 && st.Phase == "scanning" {
		scan["progress_percent"] = min(99, int(100*st.Bytes/st.Expected))
	}
	if st.Phase == "hashing" {
		scan["comparing"] = fmt.Sprintf("%d of %d size groups", st.SizeGroupsDone, st.SizeGroups)
	}
	if st.Imported != "" {
		scan["opened_report"] = st.Imported
	}
	var disks []map[string]any
	for _, d := range diskStats() {
		disks = append(disks, map[string]any{"drive": d.Drive, "free": sized(d.Free), "total": sized(d.Total)})
	}
	v := a.View()
	return map[string]any{
		"scan": scan, "disk": disks,
		"duplicates":                map[string]any{"groups": groups, "reclaimable": sized(reclaim)},
		"window":                    map[string]any{"open": a.hub.count() > 0, "tab": v.Tab, "folder": v.Path, "selected": v.Selected, "filter": v.Filter},
		"waiting_for_user_approval": len(a.pendingApprovals()),
		"have_map":                  a.mapReady(),
	}
}

func (a *App) mapReady() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tree != nil
}

func toolStatus(ctx context.Context, a *App, args map[string]any) (any, error) {
	return briefStatus(a), nil
}

func toolScan(ctx context.Context, a *App, args map[string]any) (any, error) {
	a.mu.RLock()
	cfg := a.settings
	a.mu.RUnlock()
	if roots := argStrs(args, "roots"); len(roots) > 0 {
		cfg.Roots = roots
	}
	cfg.FindDupes = argBool(args, "find_duplicates") // off unless asked for: hashing reads a lot of the disk
	if mb := argNum(args, "min_duplicate_mb", 0); mb > 0 {
		cfg.MinDup = int64(mb * (1 << 20))
	}
	if err := a.startScan(cfg, false); err != nil {
		return nil, err
	}
	a.Show("space", "", "")
	return map[string]any{"started": true, "roots": normalizeRoots(cfg.Roots), "find_duplicates": cfg.FindDupes}, nil
}

func toolWait(ctx context.Context, a *App, args map[string]any) (any, error) {
	timeout := time.Duration(min(300, max(1, int(argNum(args, "timeout_seconds", 60))))) * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		a.smu.Lock()
		running := a.running
		a.smu.Unlock()
		if !running {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return briefStatus(a), nil
}

func itemOut(t *Tree, it Item, parent int64) map[string]any {
	m := map[string]any{"name": it.Name, "kind": it.Kind, "size": sized(it.Size)}
	if parent > 0 {
		m["percent"] = float64(int(1000*it.Size/parent)) / 10
	}
	if it.Path != "" {
		m["path"] = it.Path
	}
	if it.Files > 1 {
		m["files"] = it.Files
	}
	if it.Sys {
		m["system"] = true
	}
	if it.Dom != "" && it.Dom != "other" && it.Pure >= 0.5 {
		m["mostly"] = it.Dom
	}
	if it.Dup {
		m["has_duplicate"] = true
	}
	if it.MTime > 0 {
		m["modified"] = fmtTime(it.MTime)
	}
	if it.Kind == "free" {
		m["note"] = "free space on the drive"
	}
	if it.Kind == "unacc" {
		m["note"] = "used according to Windows but not found by the scan: shadow copies, protected system areas, skipped folders"
	}
	return m
}

func (a *App) treeOrErr() (*Tree, error) {
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	if t == nil {
		return nil, fmt.Errorf("there is no space map yet: call scan first (or ask the user to open a saved report for duplicates)")
	}
	return t, nil
}

func toolFolder(ctx context.Context, a *App, args map[string]any) (any, error) {
	t, err := a.treeOrErr()
	if err != nil {
		return nil, err
	}
	f, bad := parseFilter(argStr(args, "filter"))
	limit := min(100, max(1, int(argNum(args, "limit", 30))))
	path := argStr(args, "path")
	it, crumbs, ok := t.View(path, ViewOpts{Depth: 1, Limit: limit, HideSys: argBool(args, "hide_system"), ShowFree: true, ShowUnacc: true, Filter: f, Dup: a.isDup})
	if !ok {
		return nil, fmt.Errorf("%q is not on the map (it was too small to keep, or was not scanned)", path)
	}
	items := make([]map[string]any, 0, len(it.Children))
	for _, c := range it.Children {
		items = append(items, itemOut(t, c, it.Size))
	}
	out := map[string]any{"path": path, "size": sized(it.Size), "files": it.Files, "scan_complete": it.Done, "items": items, "breadcrumb": crumbs}
	if len(bad) > 0 {
		out["ignored_filter_terms"] = bad
	}
	return out, nil
}

func (t *Tree) topFiles(path string, n int, f *Filter, hideSys bool, dup func(string) bool) ([]Item, bool) {
	root, exact := t.Root.find(path)
	if root == nil || !exact {
		return nil, false
	}
	var best []Item
	consider := func(it Item) {
		if len(best) == n && it.Size <= best[n-1].Size {
			return
		}
		i := sort.Search(len(best), func(i int) bool { return best[i].Size < it.Size })
		best = append(best, Item{})
		copy(best[i+1:], best[i:])
		best[i] = it
		if len(best) > n {
			best = best[:n]
		}
	}
	var walk func(nd *Node, base string)
	walk = func(nd *Node, base string) {
		nd.mu.Lock()
		dirs := append([]*Node(nil), nd.Dirs...)
		files := append([]FileEnt(nil), nd.Files...)
		sys := nd.Sys
		nd.mu.Unlock()
		if hideSys && sys {
			return
		}
		for _, fe := range files {
			if hideSys && fe.Sys {
				continue
			}
			p := joinPath(base, fe.Name)
			d := dup != nil && dup(p)
			if !f.Match(p, fe.Name, fe.Size, fe.MTime, d) {
				continue
			}
			consider(Item{Name: fe.Name, Path: p, Kind: "file", Size: fe.Size, MTime: fe.MTime, Dup: d, Sys: fe.Sys})
		}
		for _, d := range dirs {
			walk(d, joinPath(base, d.Name))
		}
	}
	walk(root, path)
	return best, true
}

func toolLargest(ctx context.Context, a *App, args map[string]any) (any, error) {
	t, err := a.treeOrErr()
	if err != nil {
		return nil, err
	}
	f, bad := parseFilter(argStr(args, "filter"))
	n := min(100, max(1, int(argNum(args, "count", 20))))
	items, ok := t.topFiles(argStr(args, "path"), n, f, argBool(args, "hide_system"), a.isDup)
	if !ok {
		return nil, fmt.Errorf("that folder is not on the map")
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, itemOut(t, it, 0))
	}
	res := map[string]any{"files": out, "note": "only files of 1 MB or more are tracked individually"}
	if len(bad) > 0 {
		res["ignored_filter_terms"] = bad
	}
	return res, nil
}

func toolDuplicates(ctx context.Context, a *App, args map[string]any) (any, error) {
	cat, loc, query := strings.ToLower(argStr(args, "category")), strings.ToLower(argStr(args, "location")), strings.ToLower(argStr(args, "query"))
	minSize := int64(argNum(args, "min_size_mb", 0) * (1 << 20))
	limit := min(100, max(1, int(argNum(args, "limit", 20))))
	offset := max(0, int(argNum(args, "offset", 0)))
	type row struct {
		g       *Group
		live    []*Copy
		reclaim int64
	}
	a.mu.RLock()
	var rows []row
	for _, g := range a.groups {
		lv := g.live()
		if len(lv) < 2 || g.Size < minSize {
			continue
		}
		match := cat == "" && loc == ""
		for _, c := range lv {
			if c.Keep {
				continue
			}
			okc := cat == "" || (c.Cat >= 0 && c.Cat < len(a.cats) && strings.Contains(strings.ToLower(a.cats[c.Cat].Name), cat))
			okl := loc == "" || strings.Contains(strings.ToLower(c.Loc), loc)
			if okc && okl {
				match = true
			}
		}
		if match && query != "" {
			match = false
			for _, c := range lv {
				if strings.Contains(strings.ToLower(c.Path), query) {
					match = true
				}
			}
		}
		if match {
			rows = append(rows, row{g, lv, g.Size * int64(len(lv)-1)})
		}
	}
	by := argStr(args, "sort")
	sort.Slice(rows, func(i, j int) bool {
		if by == "size" {
			return rows[i].g.Size > rows[j].g.Size
		}
		return rows[i].reclaim > rows[j].reclaim
	})
	var total int64
	for _, r := range rows {
		total += r.reclaim
	}
	end := min(len(rows), offset+limit)
	var groups []map[string]any
	if offset < len(rows) {
		for _, r := range rows[offset:end] {
			var copies []map[string]any
			for _, c := range r.live {
				cc := map[string]any{"path": c.Path, "suggested_keep": c.Keep, "modified": fmtTime(c.MTime)}
				if c.Cat >= 0 && c.Cat < len(a.cats) {
					cc["category"], cc["risk"] = a.cats[c.Cat].Name, a.cats[c.Cat].Risk
				}
				if len(c.Links) > 0 {
					cc["hardlinks"] = len(c.Links)
				}
				copies = append(copies, cc)
			}
			groups = append(groups, map[string]any{"id": r.g.ID, "file_size": sized(r.g.Size), "reclaimable": sized(r.reclaim), "copies": copies})
		}
	}
	a.mu.RUnlock()
	return map[string]any{"matching_groups": len(rows), "reclaimable_in_matches": sized(total), "showing": fmt.Sprintf("%d-%d", min(offset+1, len(rows)), end), "groups": groups}, nil
}

func toolCategories(ctx context.Context, a *App, args map[string]any) (any, error) {
	cats, _ := a.Categories()
	type stat struct{ n, b int64 }
	stats := make([]stat, len(cats))
	a.mu.RLock()
	for _, g := range a.groups {
		lv := g.live()
		if len(lv) < 2 {
			continue
		}
		for _, c := range lv {
			if !c.Keep && c.Cat >= 0 && c.Cat < len(stats) {
				stats[c.Cat].n++
				stats[c.Cat].b += g.Size
			}
		}
	}
	a.mu.RUnlock()
	out := make([]map[string]any, 0, len(cats))
	for i, c := range cats {
		out = append(out, map[string]any{"name": c.Name, "risk": c.Risk, "meaning": c.Desc, "rules": c.Rules,
			"duplicate_files_now": stats[i].n, "reclaimable_now": sized(stats[i].b)})
	}
	return map[string]any{"checked_in_this_order": true, "categories": out}, nil
}

// zipStates are the words the zips tool uses for ArchiveInfo.State.
var zipStates = map[string]string{"safe": "safe_to_remove", "different": "not_the_same", "unchecked": "not_compared_yet", "cannot": "cannot_compare"}

func toolZips(ctx context.Context, a *App, args map[string]any) (any, error) {
	list, known := a.Archives()
	if !known {
		return nil, fmt.Errorf("there is no list of zips: it comes from a scan run in the DiskScout window with duplicate search on (scan with find_duplicates, or ask the user to)")
	}
	query, only := strings.ToLower(argStr(args, "query")), argStr(args, "only")
	limit := min(max(int(argNum(args, "limit", 25)), 1), 100)
	counts := map[string]int{}
	var safeBytes int64
	var rows []map[string]any
	for _, i := range list {
		st := zipStates[i.State()]
		counts[st]++
		if st == "safe_to_remove" {
			safeBytes += i.Size
		}
		if (query != "" && !strings.Contains(strings.ToLower(i.Zip+"\n"+i.Base), query)) || (only != "" && only != st) || len(rows) >= limit {
			continue
		}
		row := map[string]any{"zip": i.Zip, "size": sized(i.Size), "state": st}
		if i.Base != "" {
			row["folder"] = i.Base
			if i.Root != "" {
				row["folder"] = i.Root
			}
		}
		switch {
		case i.Err != "":
			row["cannot_open"] = i.Err
		case !i.Readable:
			row["kind"], row["note"], row["files_in_folder"] = i.Kind, i.Note, i.FolderFiles
		default:
			row["files_in_zip"], row["same_name_and_size"] = i.Entries, i.Same
			if i.Extras >= 0 {
				row["extra_files_in_folder"] = i.Extras
			} else {
				row["extra_files_in_folder"] = "cannot be told: unpacked in among other files"
			}
			if i.Check != nil {
				row["compared"] = i.Check.Verdict
				row["compared_at"] = time.UnixMilli(i.Check.At).Format("2006-01-02 15:04")
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"unpacked_zips": len(list), "safe_to_remove": map[string]any{"count": counts["safe_to_remove"], "bytes": sized(safeBytes)},
		"not_the_same": counts["not_the_same"], "not_compared_yet": counts["not_compared_yet"], "cannot_compare": counts["cannot_compare"],
		"showing": len(rows), "zips": rows,
		"note": "not_compared_yet only means names and sizes line up: call compare_zip before saying a zip and its folder are the same. The window's Compare all button compares every zip."}, nil
}

func toolCompareZip(ctx context.Context, a *App, args map[string]any) (any, error) {
	p := filepath.Clean(argStr(args, "path"))
	if p == "." || !a.inScannedRoots(p) {
		return nil, fmt.Errorf("that path is not inside the scanned folders")
	}
	info, err := a.ArchiveFor(p)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"zip": info.Zip, "kind": info.Kind, "size": sized(info.Size)}
	if info.Err != "" {
		out["cannot_open"] = info.Err
		return out, nil
	}
	if info.Base == "" {
		out["result"] = "No folder was found that the zip was unpacked into (looked beside the zip, and where its biggest files appear elsewhere on the scanned drives). That does not prove it was never unpacked."
		if info.Note != "" {
			out["note"] = info.Note
		}
		return out, nil
	}
	if !info.Readable {
		out["folder"], out["result"] = info.Base, info.Note
		return out, nil
	}
	chk, err := a.CheckArchive(ctx, info.Zip, info.Base, info.Strip, nil)
	if err != nil {
		return nil, err
	}
	out["folder"], out["files_in_zip"], out["identical_in_folder"] = info.Base, chk.Entries, chk.Identical
	out["missing_from_folder"], out["different_in_folder"], out["could_not_read"] = chk.Missing, chk.Different, chk.Unreadable
	if chk.ExtrasKnown {
		out["in_folder_but_not_in_zip"] = chk.Extras
	} else {
		out["in_folder_but_not_in_zip"] = "cannot be told: the zip was unpacked in among other files"
	}
	out["everything_in_zip_is_in_folder"], out["everything_in_folder_is_in_zip"] = chk.ZipInFolder, chk.FolderInZip
	out["read"] = sized(chk.BytesRead)
	out["verdict"] = chk.Verdict
	return out, nil
}

func toolShow(ctx context.Context, a *App, args map[string]any) (any, error) {
	if a.hub.count() == 0 {
		return nil, fmt.Errorf("no DiskScout window is open")
	}
	a.Show(argStr(args, "tab"), argStr(args, "path"), argStr(args, "select"))
	return "ok", nil
}

func toolSuggest(ctx context.Context, a *App, args map[string]any) (any, error) {
	var in []struct{ Path, Reason string }
	if arr, ok := args["items"].([]any); ok {
		for _, v := range arr {
			if m, ok := v.(map[string]any); ok {
				in = append(in, struct{ Path, Reason string }{argStr(m, "path"), argStr(m, "reason")})
			}
		}
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("items is empty")
	}
	ok, bad := a.Suggest("Claude", in)
	var total int64
	for _, s := range ok {
		total += s.Size
	}
	return map[string]any{"added_to_clean_list": len(ok), "total": sized(total), "refused": bad, "note": "nothing was removed: the user reviews their clean list"}, nil
}

func toolRun(ctx context.Context, a *App, args map[string]any) (any, error) {
	res, err := a.RunCommand(ctx, "Claude", argStr(args, "program"), argStrs(args, "args"), argStr(args, "reason"),
		time.Duration(argNum(args, "timeout_seconds", 60))*time.Second)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func toolTrash(ctx context.Context, a *App, args map[string]any) (any, error) {
	res, err := a.RequestTrash(ctx, "Claude", argStr(args, "kind"), argStrs(args, "paths"), argStr(args, "reason"))
	if err != nil {
		return nil, err
	}
	var moved, freedNote int64
	for _, r := range res {
		if r.OK {
			moved++
			freedNote += r.Size
		}
	}
	return map[string]any{"moved_to_recycle_bin": moved, "size": sized(freedNote), "results": res,
		"note": "space comes back only when the Recycle Bin is emptied"}, nil
}

// ---------------------------------------------------------------- the stdio bridge (Claude Desktop / Claude Code CLI)

type connectionFile struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func connectionPath(dataDir string) string { return filepath.Join(dataDir, "connection.json") }

func writeConnection(dataDir string, port int, token string) {
	b, _ := json.Marshal(connectionFile{port, token, os.Getpid()})
	os.WriteFile(connectionPath(dataDir), b, 0o600)
}

func readConnection(dataDir string) (connectionFile, bool) {
	var c connectionFile
	b, err := os.ReadFile(connectionPath(dataDir))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Port == 0 {
		return c, false
	}
	return c, true
}

// launchApp starts DiskScout with its window (a variable so tests never start a real one).
var launchApp = func(dataDir string) error {
	cmd := exec.Command(selfExe(), "--data", dataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP: it must outlive the bridge
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// readStreamedResponse reads an event stream from the app: progress notifications go to notify as they arrive, and the
// JSON-RPC response (the message that has an id and no method) is returned.
func readStreamedResponse(body io.Reader, notify func([]byte)) ([]byte, error) {
	in := bufio.NewReaderSize(body, 1<<20)
	var data []string
	for {
		line, err := in.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && len(data) > 0:
			payload := []byte(strings.Join(data, "\n"))
			data = nil
			var m rpcReq
			if json.Unmarshal(payload, &m) == nil && m.Method == "" && len(m.ID) > 0 {
				return payload, nil
			}
			if notify != nil {
				notify(payload)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("DiskScout closed the connection before it answered")
		}
	}
}

// bridgeCall sends one MCP request to the running app and returns its response body. Progress notifications that the
// app streams while a long call is waiting are passed to notify.
func bridgeCall(dataDir string, line []byte, notify func([]byte)) ([]byte, error) {
	post := func(c connectionFile) ([]byte, error) {
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/mcp", c.Port), bytes.NewReader(line))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("X-Token", c.Token)
		resp, err := (&http.Client{Timeout: 25 * time.Minute}).Do(req) // an approval (up to 10 minutes) plus a command (up to 10)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("refused (stale connection)")
		}
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			return readStreamedResponse(resp.Body, notify)
		}
		return io.ReadAll(resp.Body)
	}
	if c, ok := readConnection(dataDir); ok {
		if b, err := post(c); err == nil {
			return b, nil
		}
	}
	// not running (or the file is stale): start it, with its window, and wait for it to say where it listens
	_ = os.Remove(connectionPath(dataDir))
	if err := launchApp(dataDir); err != nil {
		return nil, fmt.Errorf("could not start DiskScout: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if c, ok := readConnection(dataDir); ok {
			if b, err := post(c); err == nil {
				return b, nil
			}
		}
	}
	return nil, fmt.Errorf("DiskScout did not start in time")
}

// runMCPStdio speaks MCP on stdin/stdout. It answers the handshake and the tool list itself, so it connects at once,
// and forwards tool calls to the app.
func runMCPStdio(dataDir string) { serveMCPStdio(os.Stdin, os.Stdout, dataDir) }

func serveMCPStdio(r io.Reader, w io.Writer, dataDir string) {
	in := bufio.NewReaderSize(r, 1<<20)
	var wmu sync.Mutex
	var wg sync.WaitGroup
	write := func(b []byte) {
		var buf bytes.Buffer
		if json.Compact(&buf, b) != nil {
			return
		}
		buf.WriteByte('\n')
		wmu.Lock()
		w.Write(buf.Bytes())
		wmu.Unlock()
	}
	handle := func(line []byte) {
		defer wg.Done()
		var req rpcReq
		if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
			return // unparsable, or a notification: nothing to answer
		}
		if req.Method == "tools/call" {
			b, err := bridgeCall(dataDir, line, write)
			if err != nil {
				resp, _ := json.Marshal(rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"isError": true,
					"content": []map[string]any{{"type": "text", "text": "DiskScout is not reachable: " + err.Error()}}}})
				write(resp)
				return
			}
			if len(bytes.TrimSpace(b)) > 0 {
				write(b)
			}
			return
		}
		if resp := mcpHandle(context.Background(), nil, req); resp != nil {
			b, _ := json.Marshal(resp)
			write(b)
		}
	}
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			wg.Add(1)
			go handle(append([]byte(nil), line...))
		}
		if err != nil {
			break
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
