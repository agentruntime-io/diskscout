package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestClassifyCommand(t *testing.T) {
	cases := []struct {
		prog  string
		args  string
		class string
	}{
		{"docker", "system df", "read"},
		{"docker.exe", "system df -v", "read"},
		{"DOCKER", "System DF", "read"},
		{"docker", "ps -a", "read"},
		{`C:\Program Files\Docker\Docker\resources\bin\docker.exe`, "volume ls", "read"},
		{"docker", "system df extra", "other"}, // something rode along: no longer a plain diagnostic
		{"docker", "", "other"},
		{"docker", "system prune -a -f", "cleanup"},
		{"docker", "volume prune", "cleanup"},
		{"docker", "builder prune", "cleanup"},
		{"docker", "rm -f somecontainer", "other"},
		{"docker", "run --rm alpine rm -rf /", "other"},
		{"wsl", "-l -v", "read"},
		{"wsl", "--shutdown", "cleanup"},
		{"wsl", "-e rm -rf /", "other"},
		{"go", "env GOCACHE", "read"},
		{"go", "env GOCACHE GOMODCACHE", "read"},
		{"go", "env -w GOFLAGS=-mod=mod", "other"}, // writes Go's settings
		{"go", "env -u GOCACHE", "other"},          // unsets one: also a change, so not a diagnostic
		{"go", "version", "read"},
		{"go", "clean -n", "cleanup"},
		{"npm", "config get cache", "read"},
		{"npm", "cache clean --force", "cleanup"},
		{"npm", "install", "other"},
		{"uv", "cache dir", "read"},
		{"uv", "cache clean", "cleanup"},
		{"powercfg", "/a", "read"},
		{"powercfg", "/hibernate off", "cleanup"},
		{"vssadmin", "list shadowstorage /for=C:", "read"},
		{"vssadmin", "delete shadows /all", "other"},
		{"powershell", "-Command Remove-Item C:\\x -Recurse", "other"},
		{"cmd", "/c dir", "other"},
	}
	for _, c := range cases {
		args := strings.Fields(c.args)
		got, note := classifyCommand(c.prog, args)
		if got != c.class {
			t.Errorf("%s %s: got %s, want %s", c.prog, c.args, got, c.class)
		}
		if got != "read" && note == "" {
			t.Errorf("%s %s: a command that needs approval must say what it is", c.prog, c.args)
		}
	}
	if c, _ := classifyCommand("docker", []string{"images", "--format", "table {{.Repository}}	{{.Size}}"}); c != "read" {
		t.Errorf("a --format value is part of the flag, not a stowaway: %s", c)
	}
	if c, _ := classifyCommand("docker", []string{"images", "--format", "x", "alpine"}); c != "other" {
		t.Errorf("a positional after the flag's value is not harmless: %s", c)
	}
	if line := commandLine("docker", []string{"images", "--format", "table {{.Repository}}"}); line != `docker images --format "table {{.Repository}}"` {
		t.Errorf("command line: %s", line)
	}
}

func TestDecodeOutput(t *testing.T) {
	u16 := func(s string, bom bool) []byte {
		var b []byte
		if bom {
			b = []byte{0xFF, 0xFE}
		}
		for _, r := range utf16.Encode([]rune(s)) {
			b = append(b, byte(r), byte(r>>8))
		}
		return b
	}
	for _, bom := range []bool{false, true} {
		if got := decodeOutput(u16("  NAME      STATE\r\n* docker-desktop  Running\r\n", bom)); got != "  NAME      STATE\n* docker-desktop  Running\n" {
			t.Errorf("utf-16 (bom=%v): %q", bom, got)
		}
	}
	if decodeOutput([]byte("plain\r\ntext")) != "plain\ntext" {
		t.Error("plain text")
	}
	if got := decodeOutput([]byte{'a', 0xFF, 'b'}); got != "a?b" {
		t.Errorf("invalid utf-8: %q", got)
	}
}

func TestRealWslOutputIsReadable(t *testing.T) {
	path, err := exec.LookPath("wsl")
	if err != nil {
		t.Skip("no wsl here")
	}
	res := runProgram(context.Background(), path, []string{"-l", "-v"}, 20*time.Second)
	if res.ExitCode != 0 {
		t.Skipf("wsl is not usable here: %+v", res)
	}
	if strings.ContainsRune(res.Output, 0) || !strings.Contains(strings.ToUpper(res.Output), "NAME") {
		t.Fatalf("wsl prints UTF-16; it should have been decoded: %q", res.Output)
	}
}

func TestRunProgramLimits(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go")
	}
	old := maxCommandOutput
	maxCommandOutput = 40
	res := runProgram(context.Background(), goPath, []string{"env"}, 30*time.Second)
	maxCommandOutput = old
	if !res.Truncated || len(res.Output) > 60 {
		t.Fatalf("output must be capped: truncated=%v len=%d", res.Truncated, len(res.Output))
	}
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("no powershell")
	}
	start := time.Now()
	res = runProgram(context.Background(), ps, []string{"-NoProfile", "-Command", "Start-Sleep -Seconds 20"}, time.Second)
	if res.ExitCode != -1 || !strings.Contains(res.Output, "stopped") || time.Since(start) > 10*time.Second {
		t.Fatalf("a runaway command must be stopped: %+v after %s", res, time.Since(start))
	}
}

func TestRunCommandReadRunsAtOnce(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	a := newTestApp(t)
	res, err := a.RunCommand(context.Background(), "Claude", "go", []string{"version"}, "", 0)
	if err != nil || res.ExitCode != 0 || !strings.Contains(res.Output, "go version") || res.Class != "read" || res.Approved {
		t.Fatalf("%+v %v", res, err)
	}
	log, _ := os.ReadFile(filepath.Join(a.dataDir, "commands.log"))
	if !strings.Contains(string(log), `"command":"go version"`) || !strings.Contains(string(log), `"by":"Claude"`) || !strings.Contains(string(log), `"approved":false`) {
		t.Fatalf("commands.log: %s", log)
	}
	if _, err := a.RunCommand(context.Background(), "Claude", "definitely-not-a-program-xyz", nil, "", 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a missing program: %v", err)
	}
	if _, err := a.RunCommand(context.Background(), "Claude", "", nil, "", 0); err == nil {
		t.Fatal("an empty program is refused")
	}
}

// A call that waits for the user must not look dead: clients such as Claude Code drop a request that has not begun to answer
// after a minute. When the client accepts an event stream the reply starts at once and carries on until the answer is ready.
func TestSlowCallsAreStreamed(t *testing.T) {
	if _, err := exec.LookPath("hostname"); err != nil {
		t.Skip("no hostname")
	}
	old := mcpKeepalive
	mcpKeepalive = 40 * time.Millisecond
	defer func() { mcpKeepalive = old }()
	s := newTestServer(t)
	events, closeStream := sseReader(t, s)
	defer closeStream()
	waitEvent(t, events, "hello", nil)

	streamed := func(meta map[string]any) (*http.Response, string) {
		type out struct {
			resp *http.Response
			body string
		}
		done := make(chan out, 1)
		go func() {
			params := map[string]any{"name": "run_command", "arguments": map[string]any{"program": "hostname"}}
			if meta != nil {
				params["_meta"] = meta
			}
			resp, body := s.do(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": params},
				func(r *http.Request) { r.Header.Set("Accept", "application/json, text/event-stream") })
			done <- out{resp, string(body)}
		}()
		waitEvent(t, events, "approval", nil)
		time.Sleep(300 * time.Millisecond) // several keepalive intervals pass while the user decides
		var ap Approval
		s.app.cl.mu.Lock()
		for _, p := range s.app.cl.pending {
			ap = *p
		}
		s.app.cl.mu.Unlock()
		s.do(t, "POST", "/api/approval", map[string]any{"id": ap.ID, "approve": true})
		o := <-done
		return o.resp, o.body
	}

	resp, body := streamed(map[string]any{"progressToken": "tok-1"})
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	if !strings.HasPrefix(body, ": working\n\n") {
		t.Fatalf("the stream should open at once: %q", body)
	}
	if !strings.Contains(body, `"method":"notifications/progress"`) || !strings.Contains(body, `"progressToken":"tok-1"`) {
		t.Fatalf("progress is reported to a client that asked for it: %q", body)
	}
	if i, j := strings.Index(body, "notifications/progress"), strings.LastIndex(body, `"result"`); i < 0 || j < i || !strings.Contains(body, `approved_by_user\":true`) {
		t.Fatalf("the answer comes last and is complete: %q", body)
	}

	// no progress token: only comments, which are not messages
	_, body = streamed(nil)
	if strings.Contains(body, "notifications/progress") || !strings.Contains(body, ": still working") || !strings.Contains(body, `approved_by_user\":true`) {
		t.Fatalf("without a token: %q", body)
	}

	// the bridge reads such a stream: notifications are passed on, the response is returned
	stream := ": working\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n" +
		": still working\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":5,\"result\":{\"content\":[]}}\n\n"
	var notes []string
	got, err := readStreamedResponse(strings.NewReader(stream), func(b []byte) { notes = append(notes, string(b)) })
	if err != nil || !strings.Contains(string(got), `"id":5`) || len(notes) != 1 || !strings.Contains(notes[0], "progress") {
		t.Fatalf("bridge: %s %v %v", got, notes, err)
	}
	if _, err := readStreamedResponse(strings.NewReader(": working\n\n"), nil); err == nil {
		t.Fatal("a stream that ends without an answer is an error, not silence")
	}

	// a client that gives up takes its pending approval with it, and says so in the window
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"run_command","arguments":{"program":"hostname"}}}`))
	req.Header.Set("X-Token", "tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	go http.DefaultClient.Do(req)
	waitEvent(t, events, "approval", nil)
	cancel()
	waitEvent(t, events, "toast", func(d string) bool { return strings.Contains(d, "stopped waiting") })
	waitEvent(t, events, "approval-done", nil)
}

func TestRunCommandApproval(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	s := newTestServer(t)

	// nobody to ask: anything that is not a plain diagnostic is refused, and never runs
	if text, isErr := s.callTool(t, "run_command", map[string]any{"program": "hostname"}); !isErr || !strings.Contains(text, "no DiskScout window") {
		t.Fatalf("no window: %q", text)
	}
	events, closeStream := sseReader(t, s)
	defer closeStream()
	waitEvent(t, events, "hello", nil)

	run := func(args map[string]any) chan [2]any {
		ch := make(chan [2]any, 1)
		go func() { text, isErr := s.callTool(t, "run_command", args); ch <- [2]any{text, isErr} }()
		return ch
	}
	answer := func(approve bool) Approval {
		ev := waitEvent(t, events, "approval", nil)
		var ap Approval
		json.Unmarshal([]byte(ev), &ap)
		if ap.Kind != "command" || ap.Command == "" || ap.Class == "" || ap.Note == "" || ap.Actor != "Claude" {
			t.Fatalf("what the user is shown: %+v", ap)
		}
		s.do(t, "POST", "/api/approval", map[string]any{"id": ap.ID, "approve": approve, "trust": true}) // trust is ignored for commands
		waitEvent(t, events, "approval-done", nil)
		return ap
	}

	// a cleanup command waits for the user, then runs
	r := run(map[string]any{"program": "go", "args": []any{"clean", "-n"}, "reason": "dry run"})
	ap := answer(true)
	if ap.Class != "cleanup" || ap.Command != "go clean -n" {
		t.Fatalf("approval: %+v", ap)
	}
	got := <-r
	if got[1].(bool) || !strings.Contains(got[0].(string), `"approved_by_user":true`) || !strings.Contains(got[0].(string), `"exit_code":0`) {
		t.Fatalf("approved: %v", got)
	}

	// an unlisted command is flagged as such, and declining stops it
	r = run(map[string]any{"program": "hostname"})
	if ap := answer(false); ap.Class != "other" {
		t.Fatalf("class: %+v", ap)
	}
	if got := <-r; !got[1].(bool) || !strings.Contains(got[0].(string), "declined") {
		t.Fatalf("declined: %v", got)
	}

	// "don't ask again" does not exist for commands: the next one asks again
	r = run(map[string]any{"program": "hostname"})
	answer(true)
	host, _ := os.Hostname()
	if got := <-r; got[1].(bool) || !strings.Contains(strings.ToLower(got[0].(string)), strings.ToLower(host)) {
		t.Fatalf("hostname: %v", got)
	}
	r = run(map[string]any{"program": "hostname"})
	waitEvent(t, events, "approval", nil) // it asked again; nobody answers, so end it
	s.app.cl.mu.Lock()
	for _, p := range s.app.cl.pending {
		p.res <- approvalDecision{approved: false}
	}
	s.app.cl.mu.Unlock()
	<-r

	// a diagnostic needs no one at all
	got = <-run(map[string]any{"program": "go", "args": []any{"version"}})
	if got[1].(bool) || !strings.Contains(got[0].(string), `"class":"read"`) {
		t.Fatalf("read: %v", got)
	}
}
