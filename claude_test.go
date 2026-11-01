package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// callTool makes an MCP tools/call through the HTTP endpoint and returns the text and whether it was an error.
func (s *testServer) callTool(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	_, out := s.do(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var r struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct{ Text string }
		}
		Error *rpcErr
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%s: bad response %s", name, out)
	}
	if r.Error != nil {
		return r.Error.Message, true
	}
	if len(r.Result.Content) == 0 {
		t.Fatalf("%s: no content in %s", name, out)
	}
	return r.Result.Content[0].Text, r.Result.IsError
}

func (s *testServer) toolJSON(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := s.callTool(t, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("%s did not return JSON: %s", name, text)
	}
	return m
}

func fakeTrash(t *testing.T) {
	old := trashFile
	trashFile = func(p string) error { return os.Remove(p) } // never touch the real Recycle Bin from a test
	t.Cleanup(func() { trashFile = old })
}

func TestMCPProtocol(t *testing.T) {
	s := newTestServer(t)
	rpc := func(body any) (int, map[string]any) {
		resp, out := s.do(t, "POST", "/mcp", body)
		var m map[string]any
		json.Unmarshal(out, &m)
		return resp.StatusCode, m
	}
	code, m := rpc(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05"}})
	res := m["result"].(map[string]any)
	if code != 200 || res["protocolVersion"] != "2024-11-05" || res["serverInfo"].(map[string]any)["name"] != "diskscout" || !strings.Contains(res["instructions"].(string), "Never remove") {
		t.Fatalf("initialize: %v", m)
	}
	_, m = rpc(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": map[string]any{"protocolVersion": "1999-01-01"}})
	if m["result"].(map[string]any)["protocolVersion"] != mcpVersions[0] {
		t.Fatalf("an unknown client version should get the newest one we speak: %v", m)
	}
	code, _ = rpc(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if code != 202 {
		t.Fatalf("a notification gets 202 and no body, got %d", code)
	}
	_, m = rpc(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"})
	var names []string
	for _, tool := range m["result"].(map[string]any)["tools"].([]any) {
		tm := tool.(map[string]any)
		names = append(names, tm["name"].(string))
		if tm["inputSchema"].(map[string]any)["type"] != "object" || tm["description"] == "" {
			t.Errorf("tool %v has no proper schema or description", tm["name"])
		}
	}
	want := "status,scan,wait_for_scan,folder,largest_files,duplicates,compare_zip,zips,categories,show,suggest_cleanup,run_command,trash"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools: %v", names)
	}
	_, m = rpc(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "nope"})
	if m["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", m)
	}
	if text, isErr := s.callTool(t, "no_such_tool", nil); !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("unknown tool: %q", text)
	}
	// a batch is answered as a batch, notifications left out
	resp, out := s.do(t, "POST", "/mcp", []any{
		map[string]any{"jsonrpc": "2.0", "id": 10, "method": "ping"},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 11, "method": "ping"},
	})
	var batch []map[string]any
	json.Unmarshal(out, &batch)
	if resp.StatusCode != 200 || len(batch) != 2 {
		t.Fatalf("batch: %d %s", resp.StatusCode, out)
	}
	// the endpoint is behind the token like everything else, and also takes a Bearer header
	if r, _ := s.do(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, func(r *http.Request) { r.Header.Del("X-Token") }); r.StatusCode != 403 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r, _ := s.do(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}, func(r *http.Request) {
		r.Header.Del("X-Token")
		r.Header.Set("Authorization", "Bearer tok")
	}); r.StatusCode != 200 {
		t.Fatalf("bearer token: %d", r.StatusCode)
	}
	if r, _ := s.do(t, "GET", "/mcp", nil); r.StatusCode != 405 {
		t.Fatalf("GET /mcp: %d", r.StatusCode)
	}
}

func TestMCPTools(t *testing.T) {
	s := newTestServer(t)
	root, c := fixture(t)
	write(t, filepath.Join(root, "big.bin"), 2*MiB, 9)
	events, closeStream := sseReader(t, s)
	defer closeStream()
	waitEvent(t, events, "hello", nil)

	if st := s.toolJSON(t, "status", nil); st["have_map"] != false || st["window"].(map[string]any)["open"] != true {
		t.Fatalf("status before a scan: %v", st)
	}
	if text, isErr := s.callTool(t, "folder", nil); !isErr || !strings.Contains(text, "call scan first") {
		t.Fatalf("folder before a scan should say what to do: %q", text)
	}
	started := s.toolJSON(t, "scan", map[string]any{"roots": []any{root}, "find_duplicates": true, "min_duplicate_mb": 0.001})
	if started["started"] != true {
		t.Fatalf("scan: %v", started)
	}
	if st := s.toolJSON(t, "wait_for_scan", map[string]any{"timeout_seconds": 30}); st["scan"].(map[string]any)["phase"] != "done" {
		t.Fatalf("wait_for_scan: %v", st)
	}

	f := s.toolJSON(t, "folder", map[string]any{"path": root})
	if f["size"].(map[string]any)["bytes"].(float64) != float64(4*400*1024+2*MiB) {
		t.Fatalf("folder size: %v", f["size"])
	}
	first := f["items"].([]any)[0].(map[string]any)
	if first["name"] != "big.bin" || first["kind"] != "file" || first["percent"].(float64) <= 0 {
		t.Fatalf("folder items: %v", f["items"])
	}
	top := s.toolJSON(t, "largest_files", map[string]any{"count": 5})
	if files := top["files"].([]any); len(files) != 1 || files[0].(map[string]any)["name"] != "big.bin" {
		t.Fatalf("largest_files only tracks 1 MB and up: %v", top)
	}
	if f := s.toolJSON(t, "folder", map[string]any{"path": root, "filter": "*.zzz >5zz"}); f["ignored_filter_terms"] == nil || len(f["items"].([]any)) != 0 {
		t.Fatalf("a filter that matches nothing, with a bad term reported: %v", f)
	}

	d := s.toolJSON(t, "duplicates", nil)
	groups := d["groups"].([]any)
	if d["matching_groups"].(float64) != 1 || len(groups) != 1 {
		t.Fatalf("duplicates: %v", d)
	}
	keeps := 0
	for _, cp := range groups[0].(map[string]any)["copies"].([]any) {
		cm := cp.(map[string]any)
		if cm["suggested_keep"] == true {
			keeps++
		}
		if cm["category"] == nil || cm["risk"] == nil {
			t.Errorf("a copy without a category: %v", cm)
		}
	}
	if keeps != 1 {
		t.Fatalf("%d suggested keepers", keeps)
	}
	if none := s.toolJSON(t, "duplicates", map[string]any{"category": "downloads"}); none["matching_groups"].(float64) != 0 {
		t.Fatalf("category filter: %v", none)
	}
	if temp := s.toolJSON(t, "duplicates", map[string]any{"category": "temp", "min_size_mb": 0.1}); temp["matching_groups"].(float64) != 1 {
		t.Fatalf("temp category: %v", temp)
	}
	if cats := s.toolJSON(t, "categories", nil)["categories"].([]any); len(cats) != 6 || cats[0].(map[string]any)["risk"] == "" {
		t.Fatalf("categories: %v", cats)
	}

	// Claude moving the user's window
	if text, isErr := s.callTool(t, "show", map[string]any{"tab": "space", "path": root, "select": c[0]}); isErr || text != "ok" {
		t.Fatalf("show: %q", text)
	}
	ui := waitEvent(t, events, "ui", func(d string) bool { return strings.Contains(d, "keep") }) // not the one scan sent
	if !strings.Contains(ui, `"tab":"space"`) {
		t.Fatalf("ui event: %s", ui)
	}

	// suggestions go on the user's list; protected places are refused; nothing is removed
	sg := s.toolJSON(t, "suggest_cleanup", map[string]any{"items": []any{
		map[string]any{"path": filepath.Join(root, "big.bin"), "reason": "a big file nobody opened"},
		map[string]any{"path": os.Getenv("SystemRoot"), "reason": "no"},
	}})
	if sg["added_to_clean_list"].(float64) != 1 || len(sg["refused"].([]any)) != 1 {
		t.Fatalf("suggest: %v", sg)
	}
	sug := waitEvent(t, events, "suggest", nil)
	if !strings.Contains(sug, "a big file nobody opened") || !strings.Contains(sug, `"by":"Claude"`) {
		t.Fatalf("suggest event: %s", sug)
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin")); err != nil {
		t.Fatal("suggesting must not remove anything")
	}
	if got := s.app.State().Suggestions; len(got) != 1 {
		t.Fatalf("a window that opens later must still see the suggestion: %v", got)
	}
	s.do(t, "POST", "/api/suggestions/ack", map[string]any{})
	if len(s.app.State().Suggestions) != 0 {
		t.Fatal("acknowledged suggestions are cleared")
	}
}

func TestShowNeedsAWindow(t *testing.T) {
	s := newTestServer(t)
	if text, isErr := s.callTool(t, "show", map[string]any{"tab": "dups"}); !isErr || !strings.Contains(text, "no DiskScout window") {
		t.Fatalf("show with no window: %q", text)
	}
}

func TestApprovalGate(t *testing.T) {
	fakeTrash(t)
	s := newTestServer(t)
	root, c := fixture(t)
	scanApp(t, s.app, root)
	events, closeStream := sseReader(t, s)
	defer closeStream()
	waitEvent(t, events, "hello", nil)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	ask := func(paths ...string) chan [2]any {
		ch := make(chan [2]any, 1)
		go func() {
			text, isErr := s.callTool(t, "trash", map[string]any{"kind": "duplicates", "paths": paths, "reason": "old installers"})
			ch <- [2]any{text, isErr}
		}()
		return ch
	}
	answer := func(approve, trust bool) {
		ev := waitEvent(t, events, "approval", nil)
		var ap Approval
		json.Unmarshal([]byte(ev), &ap)
		if ap.Actor != "Claude" || ap.Reason != "old installers" || len(ap.Items) == 0 || ap.Total != int64(len(ap.Items))*400*1024 {
			t.Fatalf("approval shown to the user: %+v", ap)
		}
		if p := s.app.State().Pending; len(p) != 1 || p[0].ID != ap.ID {
			t.Fatalf("a window that opens now must see the pending request: %+v", p)
		}
		if r, _ := s.do(t, "POST", "/api/approval", map[string]any{"id": ap.ID, "approve": approve, "trust": trust}); r.StatusCode != 200 {
			t.Fatalf("answering: %d", r.StatusCode)
		}
		waitEvent(t, events, "approval-done", nil)
	}

	// 1. declined: nothing happens, and Claude is told
	r := ask(c[1])
	answer(false, false)
	if got := <-r; !got[1].(bool) || !strings.Contains(got[0].(string), "declined") || !exists(c[1]) {
		t.Fatalf("declined: %v, file exists=%v", got, exists(c[1]))
	}
	if len(s.app.State().Pending) != 0 {
		t.Fatal("nothing should be pending after an answer")
	}

	// 2. approved, and "don't ask again" ticked
	r = ask(c[1])
	answer(true, true)
	got := <-r
	if got[1].(bool) || !strings.Contains(got[0].(string), `"moved_to_recycle_bin":1`) || exists(c[1]) {
		t.Fatalf("approved: %v, file exists=%v", got, exists(c[1]))
	}
	log, _ := os.ReadFile(filepath.Join(s.app.dataDir, "deleted.log"))
	if !strings.Contains(string(log), `"by":"Claude"`) || !strings.Contains(string(log), `"mode":"trash"`) {
		t.Fatalf("the log should say Claude asked and the user approved: %s", log)
	}

	// 3. trusted: no prompt, it just happens (the user still sees a toast, and it is logged)
	r = ask(c[2])
	select {
	case got := <-r:
		if got[1].(bool) || exists(c[2]) {
			t.Fatalf("trusted request: %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a trusted request should not wait for an answer")
	}
	if !exists(c[0]) {
		t.Fatal("the last copy is still guarded")
	}
}

func TestApprovalEdgeCases(t *testing.T) {
	fakeTrash(t)
	s := newTestServer(t)
	root, c := fixture(t)
	scanApp(t, s.app, root)

	// no window open: nobody can approve, so nothing is even queued
	if text, isErr := s.callTool(t, "trash", map[string]any{"kind": "duplicates", "paths": []any{c[1]}}); !isErr || !strings.Contains(text, "no DiskScout window") || !pathExists(c[1]) {
		t.Fatalf("no window: %q", text)
	}
	events, closeStream := sseReader(t, s)
	defer closeStream()
	waitEvent(t, events, "hello", nil)

	// things that are not offered at all never reach the user as a request
	res := s.toolJSON(t, "trash", map[string]any{"kind": "paths", "paths": []any{os.Getenv("SystemRoot"), `C:\`, filepath.Join(t.TempDir(), "elsewhere.bin")}})
	if res["moved_to_recycle_bin"].(float64) != 0 || len(res["results"].([]any)) != 3 {
		t.Fatalf("protected and unscanned paths: %v", res)
	}
	if text, isErr := s.callTool(t, "trash", map[string]any{"kind": "duplicates", "paths": []any{filepath.Join(root, "unique.bin")}}); isErr || !strings.Contains(text, "not a duplicate") {
		t.Fatalf("a file that is not a duplicate: %q", text)
	}
	if text, isErr := s.callTool(t, "trash", map[string]any{"kind": "everything", "paths": []any{c[1]}}); !isErr || !strings.Contains(text, "kind must be") {
		t.Fatalf("bad kind: %q", text)
	}

	// an unanswered request times out and removes nothing
	old := approvalTimeout
	approvalTimeout = 300 * time.Millisecond
	defer func() { approvalTimeout = old }()
	text, isErr := s.callTool(t, "trash", map[string]any{"kind": "duplicates", "paths": []any{c[1]}})
	if !isErr || !strings.Contains(text, "did not answer") || !pathExists(c[1]) || len(s.app.State().Pending) != 0 {
		t.Fatalf("timeout: %q", text)
	}
	approvalTimeout = old

	// asking for every copy: even an approved request cannot remove the last one
	done := make(chan string, 1)
	go func() {
		text, _ := s.callTool(t, "trash", map[string]any{"kind": "duplicates", "paths": []any{c[0], c[1], c[2]}})
		done <- text
	}()
	ev := waitEvent(t, events, "approval", func(d string) bool { // skip the stale one from the request that timed out
		var a Approval
		json.Unmarshal([]byte(d), &a)
		return len(a.Items) == 3
	})
	var ap Approval
	json.Unmarshal([]byte(ev), &ap)
	s.do(t, "POST", "/api/approval", map[string]any{"id": ap.ID, "approve": true})
	if text := <-done; strings.Contains(text, `"moved_to_recycle_bin":3`) || !pathExists(c[0]) || !strings.Contains(text, "every remaining copy") {
		t.Fatalf("the last-copy guard: %s", text)
	}
	if r, _ := s.do(t, "POST", "/api/approval", map[string]any{"id": "nope", "approve": true}); r.StatusCode != 404 {
		t.Fatalf("answering an unknown request: %d", r.StatusCode)
	}
}

func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestConnectDesktopConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")
	os.WriteFile(path, []byte(`{"theme":"dark","mcpServers":{"other":{"command":"node","args":["x.js"]}}}`), 0o644)
	original, _ := os.ReadFile(path)

	if err := connectDesktop(path); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readJSONMap(path)
	servers := cfg["mcpServers"].(map[string]any)
	entry := servers[mcpName].(map[string]any)
	if cfg["theme"] != "dark" || servers["other"] == nil || entry["command"] != selfExe() || entry["args"].([]any)[0] != "--mcp" {
		t.Fatalf("the user's other settings must survive and ours must be added: %v", cfg)
	}
	if bak, err := os.ReadFile(path + ".diskscout-backup"); err != nil || !bytes.Equal(bak, original) {
		t.Fatal("the user's original file should be kept as a backup")
	}
	if present, current := entryOf(cfg, selfExe()); !present || !current {
		t.Fatal("entryOf should recognise this program")
	}
	if present, current := entryOf(cfg, `C:\somewhere\else\DiskScout.exe`); !present || current {
		t.Fatal("an entry for a different copy is stale")
	}
	if err := connectDesktop(path); err != nil { // connecting twice changes nothing and keeps the first backup
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(path + ".diskscout-backup"); !bytes.Equal(bak, original) {
		t.Fatal("the backup must stay the user's original")
	}
	if err := disconnectDesktop(path); err != nil {
		t.Fatal(err)
	}
	cfg, _ = readJSONMap(path)
	if servers := cfg["mcpServers"].(map[string]any); servers[mcpName] != nil || servers["other"] == nil {
		t.Fatalf("disconnect removes only ours: %v", cfg)
	}

	// a config that is not valid JSON is left exactly as it was
	broken := filepath.Join(dir, "broken.json")
	os.WriteFile(broken, []byte(`{"mcpServers": {`), 0o644)
	if err := connectDesktop(broken); err == nil {
		t.Fatal("must refuse a config it cannot read")
	}
	if b, _ := os.ReadFile(broken); string(b) != `{"mcpServers": {` {
		t.Fatal("a broken config must not be modified")
	}
	// no config yet: create one
	fresh := filepath.Join(dir, "new", "claude_desktop_config.json")
	if err := connectDesktop(fresh); err != nil {
		t.Fatal(err)
	}
	if cfg, err := readJSONMap(fresh); err != nil || cfg["mcpServers"] == nil {
		t.Fatalf("a new config: %v %v", cfg, err)
	}
	info := claudeInfo() // reads this machine's real state: only check it is well formed
	if !strings.Contains(info.Command, `--mcp`) || !strings.Contains(info.Snippet, `"--mcp"`) || info.Exe == "" {
		t.Fatalf("info: %+v", info)
	}
}

func TestChatPlumbing(t *testing.T) {
	dir := t.TempDir()
	// a stand-in for the claude command: answers every line it is sent, in the stream-json shape the real one uses
	fake := "@echo off\r\n:loop\r\nset \"line=\"\r\nset /p line=\r\nif not defined line goto end\r\n" +
		`echo {"type":"assistant","message":{"content":[{"type":"text","text":"hello from the fake"},{"type":"tool_use","name":"mcp__diskscout__status","input":{}}]}}` + "\r\n" +
		`echo {"type":"result","subtype":"success","result":"hello from the fake","is_error":false}` + "\r\n" +
		"goto loop\r\n:end\r\n"
	os.WriteFile(filepath.Join(dir, "claude.cmd"), []byte(fake), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := newTestApp(t)
	a.port = 12345
	defer a.chat.Close()
	if !a.chat.state()["available"].(bool) {
		t.Fatal("the fake claude should be found on PATH")
	}
	waitFor := func(n int) []ChatMsg {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			h := a.chat.History()
			if len(h) >= n && !a.chat.state()["busy"].(bool) {
				return h
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("chat did not reach %d messages: %+v", n, a.chat.History())
		return nil
	}
	if err := a.chat.Send("   "); err == nil {
		t.Fatal("an empty message must be refused")
	}
	if err := a.chat.Send("what is using my disk?"); err != nil {
		t.Fatal(err)
	}
	h := waitFor(3)
	if h[0].Role != "user" || h[0].Text != "what is using my disk?" || h[1].Role != "assistant" || h[1].Text != "hello from the fake" || h[2].Role != "tool" || h[2].Tool != "status" {
		t.Fatalf("history: %+v", h)
	}
	if a.chat.sent != "what is using my disk?" {
		t.Fatalf("the first message goes as typed: %q", a.chat.sent)
	}
	if err := a.chat.Send("and again"); err != nil { // the same process keeps the conversation going
		t.Fatal(err)
	}
	if h = waitFor(6); h[3].Text != "and again" {
		t.Fatalf("second turn: %+v", h)
	}
	if a.chat.sent != "and again" {
		t.Fatalf("a process that is still running remembers by itself, so it is not told again: %q", a.chat.sent)
	}
	a.chat.Stop()
	if a.chat.state()["running"].(bool) {
		t.Fatal("Stop should end the process")
	}
	if err := a.chat.Send("continue"); err != nil { // and the next message starts a new process
		t.Fatal(err)
	}
	waitFor(9)
	// ...which knows nothing, so it must be told what was said, or "continue" means nothing to it
	for _, want := range []string{"restarted", "User: what is using my disk?", "You: hello from the fake", "(You called status", "User: and again",
		"[The user pressed Stop.]", "The user's new message:\ncontinue"} {
		if !strings.Contains(a.chat.sent, want) {
			t.Fatalf("after a restart the new process must be given %q, got:\n%s", want, a.chat.sent)
		}
	}
	if h := a.chat.History(); h[len(h)-3].Text != "continue" || strings.Contains(h[len(h)-3].Text, "restarted") {
		t.Fatalf("the panel shows only what the user typed: %+v", h[len(h)-3])
	}
	if err := a.chat.Send("one more"); err != nil {
		t.Fatal(err)
	}
	waitFor(12)
	if a.chat.sent != "one more" {
		t.Fatalf("it is told once, not on every message: %q", a.chat.sent)
	}
	a.chat.Reset()
	if len(a.chat.History()) != 0 {
		t.Fatal("Reset forgets the conversation")
	}

	// errors from Claude reach the window, and a missing claude is explained rather than crashing
	a.chat.handleLine([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key. Please run /login"}`))
	if h := a.chat.History(); len(h) != 1 || h[0].Role != "error" || !strings.Contains(h[0].Text, "/login") {
		t.Fatalf("error line: %+v", h)
	}
	a.chat.handleLine([]byte(`not json at all`))
	a.chat.handleLine([]byte(`{"type":"system","subtype":"init"}`))
	if len(a.chat.History()) != 1 {
		t.Fatal("noise must be ignored")
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if err := a.chat.Send("hello"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("with no claude: %v", err)
	}
	if env := chatEnv(); len(env) == 0 {
		t.Fatal("chatEnv dropped everything")
	}
	t.Setenv("CLAUDECODE", "1")
	for _, e := range chatEnv() {
		if strings.HasPrefix(strings.ToUpper(e), "CLAUDECODE=") {
			t.Fatal("a DiskScout started from a Claude session must not pass that on")
		}
	}
	if strings.ContainsAny(chatPromptInline, "\"\n%^&|<>") {
		t.Fatalf("the inline prompt must be safe for batch files: %q", chatPromptInline)
	}
}

// An older Claude Code does not know every flag. The chat must still start, and still have none of Claude's own tools.
func TestChatArgsAdaptToTheInstalledClaude(t *testing.T) {
	has := func(args []string, want ...string) bool {
		for i := range args {
			if i+len(want) <= len(args) && strings.Join(args[i:i+len(want)], "\x00") == strings.Join(want, "\x00") {
				return true
			}
		}
		return false
	}
	full := "--tools --allowedTools --disallowedTools --strict-mcp-config --no-session-persistence --setting-sources --append-system-prompt"
	a, err := chatArgs(full, "cfg.json", []string{"--append-system-prompt", "p"})
	if err != nil || !has(a, "--tools", "") || !has(a, "--strict-mcp-config") || !has(a, "--no-session-persistence") || !has(a, "--setting-sources", "project") ||
		!has(a, "--allowedTools", "mcp__"+mcpName) || has(a, "--disallowedTools") {
		t.Fatalf("a current claude: %v %v", a, err)
	}
	// a help that could not be read: assume the current flags, as before
	if a, err = chatArgs("", "cfg.json", nil); err != nil || !has(a, "--tools", "") {
		t.Fatalf("unreadable help: %v %v", a, err)
	}
	// no --tools (the error from the other machine), but it can name tools to refuse: all of them are refused
	old := "--allowedTools --disallowedTools --mcp-config --append-system-prompt"
	a, err = chatArgs(old, "cfg.json", nil)
	if err != nil || has(a, "--tools") || !has(a, "--disallowedTools", builtinTools) || !has(a, "--allowedTools", "mcp__"+mcpName) ||
		has(a, "--strict-mcp-config") || has(a, "--no-session-persistence") || has(a, "--setting-sources", "project") {
		t.Fatalf("an older claude: %v %v", a, err)
	}
	for _, tool := range []string{"Bash", "Edit", "Write", "Read", "WebFetch"} {
		if !strings.Contains(builtinTools, tool) {
			t.Fatalf("%s must be on the refused list", tool)
		}
	}
	// the newer spelling of the flag counts too
	if a, err = chatArgs("--disallowed-tools --allowedTools", "cfg.json", nil); err != nil || !has(a, "--disallowedTools", builtinTools) {
		t.Fatalf("%v %v", a, err)
	}
	// nothing that can limit its tools: do not start it at all
	if _, err = chatArgs("--allowedTools --mcp-config", "cfg.json", nil); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("a claude that cannot be limited must not be started: %v", err)
	}
}

// An old Claude Code asks for a model that has been retired and gets a 404, three times over. The user should see one clear
// error that says what it means, not three copies of a JSON blob that look like Claude's own answer.
func TestChatExplainsAnOldClaudeAndShowsAnErrorOnce(t *testing.T) {
	a := newTestApp(t)
	raw := `API Error: 404 {"type":"error","error":{"type":"not_found_error","message":"model: claude-3-5-haiku-20241022"},"request_id":"req_x"}`
	assistant := `{"type":"assistant","message":{"content":[{"type":"text","text":` + strconv.Quote(raw) + `}]}}`
	result := `{"type":"result","subtype":"success","is_error":true,"result":` + strconv.Quote(raw) + `}`
	for i := 0; i < 3; i++ {
		a.chat.handleLine([]byte(assistant))
	}
	a.chat.handleLine([]byte(result))
	h := a.chat.History()
	if len(h) != 1 || h[0].Role != "error" {
		t.Fatalf("one error, not an answer and not three copies: %+v", h)
	}
	for _, want := range []string{"claude-3-5-haiku-20241022", "no longer exists", "old version of Claude Code", "claude update", "ANTHROPIC_SMALL_FAST_MODEL"} {
		if !strings.Contains(h[0].Text, want) {
			t.Fatalf("the explanation should mention %q:\n%s", want, h[0].Text)
		}
	}
	// real answers are left alone, and the same error in a later turn is shown again
	a.chat.handleLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`))
	a.chat.handleLine([]byte(assistant))
	if h = a.chat.History(); len(h) != 3 || h[1].Role != "assistant" || h[1].Text != "hello" || h[2].Role != "error" {
		t.Fatalf("%+v", h)
	}
	// a missing sign-in is explained too, and keeps the original words
	if got := explainChatError("Invalid API key · Please run /login"); !strings.Contains(got, "Please run /login") || !strings.Contains(got, "not signed in") {
		t.Fatalf("%s", got)
	}
	if explainChatError("something else went wrong") != "something else went wrong" {
		t.Fatal("errors we do not know are passed on as they are")
	}
}

func TestReplayTextIsWhatWasSaid(t *testing.T) {
	if replayText(nil) != "" || replayText([]ChatMsg{{Role: "error", Text: "boom"}, {Role: "system", Text: "Stopped."}}) != "" {
		t.Fatal("with nothing said there is nothing to replay")
	}
	msgs := []ChatMsg{{Role: "user", Text: "docker is running"}, {Role: "tool", Tool: "run_command", Text: `{"program":"docker"}`},
		{Role: "system", Text: "Stopped."}, {Role: "error", Text: "Claude stopped unexpectedly."}}
	got := replayText(msgs)
	for _, want := range []string{"User: docker is running", `(You called run_command {"program":"docker"})`, "[The user pressed Stop.]", "Claude stopped unexpectedly"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "docker is running") > strings.Index(got, "pressed Stop") {
		t.Fatal("it must keep the order things happened in")
	}
	// a long conversation keeps its most recent part and says it left the start out
	var long []ChatMsg
	for i := 0; i < 400; i++ {
		long = append(long, ChatMsg{Role: "user", Text: strings.Repeat("x", 200) + " #" + strconv.Itoa(i)})
	}
	g := replayText(long)
	if len(g) > maxReplay+200 || !strings.HasPrefix(g, "(Earlier messages are left out.)") || !strings.Contains(g, "#399") || strings.Contains(g, "#0\n") {
		t.Fatalf("a long conversation keeps the end (%d chars)", len(g))
	}
	huge := replayText([]ChatMsg{{Role: "user", Text: "q"}, {Role: "assistant", Text: strings.Repeat("y", 3*maxReplay)}})
	if len(huge) > maxReplay+200 {
		t.Fatalf("one enormous message must not blow the limit: %d", len(huge))
	}
}

func TestStdioBridge(t *testing.T) {
	s := newTestServer(t)
	root, _ := fixture(t)
	scanApp(t, s.app, root)
	port := s.Listener.Addr().String()
	dataDir := t.TempDir()
	writeConnection(dataDir, atoiPort(t, port), "tok")

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"status","arguments":{}}}`,
		`not json`,
	}, "\n") + "\n"
	var out bytes.Buffer
	serveMCPStdio(strings.NewReader(in), &out, dataDir)
	got := map[float64]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a line of output is not JSON: %q", line)
		}
		got[m["id"].(float64)] = m
	}
	if len(got) != 3 {
		t.Fatalf("expected answers for 3 requests only (no answer to the notification or the junk), got %d: %s", len(got), out.String())
	}
	if got[1]["result"].(map[string]any)["serverInfo"] == nil || len(got[2]["result"].(map[string]any)["tools"].([]any)) != 13 {
		t.Fatalf("the bridge answers the handshake and the tool list by itself: %v", got)
	}
	text := got[3]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"phase":"done"`) {
		t.Fatalf("a tool call is forwarded to the running app: %s", text)
	}

	// the app is not running: the bridge starts it (here, a stand-in that fails) and reports the problem to Claude
	var launched atomic.Int32
	old := launchApp
	launchApp = func(string) error { launched.Add(1); return errors.New("pretend failure") }
	defer func() { launchApp = old }()
	empty := t.TempDir()
	out.Reset()
	serveMCPStdio(strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"status","arguments":{}}}`+"\n"), &out, empty)
	if launched.Load() != 1 || !strings.Contains(out.String(), "could not start DiskScout") || !strings.Contains(out.String(), `"isError":true`) {
		t.Fatalf("launch=%d out=%s", launched.Load(), out.String())
	}
	// a stale connection file (nothing listening there any more) behaves the same way
	writeConnection(empty, 1, "old")
	out.Reset()
	serveMCPStdio(strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"status","arguments":{}}}`+"\n"), &out, empty)
	if launched.Load() != 2 {
		t.Fatalf("a stale connection should trigger a start, launches=%d", launched.Load())
	}
}

func atoiPort(t *testing.T, hostport string) int {
	t.Helper()
	i := strings.LastIndex(hostport, ":")
	n := 0
	for _, ch := range hostport[i+1:] {
		n = n*10 + int(ch-'0')
	}
	return n
}
