package main

// The chat panel: Claude, inside the window. It runs the user's own Claude Code (`claude`) as a child process in
// streaming mode, with nothing but DiskScout's tools: no shell, no file access. Whatever Claude can do here it does
// through the same tools (and the same approval step) that Claude Desktop or a terminal session would use.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const chatPrompt = `You are Claude, built into DiskScout, a Windows tool that shows where disk space went and finds duplicate files. The user wants to free space safely.

You only have DiskScout's tools (status, folder, largest_files, duplicates, zips, compare_zip, categories, show, suggest_cleanup, trash, scan, wait_for_scan, run_command). Start with status. Look before you advise; use show to move the user's window to whatever you are talking about.

To judge a zip against the folder it was unpacked into, call zips first: it is the same list the user sees in the Zips view, with what has already been compared. Then call compare_zip on the ones that matter and have not been compared. Never say a zip and a folder are the same, or that one can go, without compare_zip; say plainly what it says it cannot tell. RAR and 7z files cannot be compared.

Be concrete and brief: sizes, paths, what each thing is, and what happens if it goes. Separate "safe" from "check first". Say plainly when you are not sure what something is.

Formatting: whenever you name a file or folder, write its full absolute path inside backticks, such as C:\Users\Sam\Downloads\setup.exe; the window turns it into a link that shows it in Explorer. When you compare several items, use a markdown table with at most four columns, such as Item (the full path), Size, What it is, Risk. Always write sizes with a unit, like 34.4 GB.

When DiskScout cannot see inside something (a Docker disk, a WSL disk, a tool cache), look with run_command first: for Docker run docker system df -v, then explain what is using the space. One command per call, no chaining. Read-only diagnostics run at once; any cleanup (docker system prune, go clean, npm cache clean) waits for the user to approve the exact command in the window, so say what it will free and what it cannot undo, and never add flags such as --volumes unless the user asked.

Never delete on your own initiative. Put proposals on the user's list with suggest_cleanup. Call trash only when the user has clearly asked you to remove specific things; it only moves them to the Recycle Bin, and the user approves each request in the DiskScout window, so tell them to look there. The Recycle Bin frees no space until it is emptied. Never propose removing Windows, Program Files, drivers or anything you cannot identify. For the pagefile or hibernation file, explain the Windows setting instead.`

// chatPromptInline is the prompt as one line without quotes, for a claude that cannot read it from a file: npm installs
// put a claude.cmd batch file in front of it, and batch files mangle quotes and line breaks in arguments.
var chatPromptInline = strings.NewReplacer("\r", "", "\n", " ", `"`, "'", "%", " percent", "^", "", "&", "and", "|", "/", "<", "", ">", "").Replace(chatPrompt)

var helpCache sync.Map // program -> what its --help printed

// cliHelp is what the installed claude prints for --help (asked once per program).
func cliHelp(cli string) string {
	if v, ok := helpCache.Load(cli); ok {
		return v.(string)
	}
	cmd := exec.Command(cli, "--help")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, _ := cmd.CombinedOutput()
	helpCache.Store(cli, string(out))
	return string(out)
}

// cliSupports asks the installed claude whether it knows a flag.
func cliSupports(cli, flag string) bool { return strings.Contains(cliHelp(cli), flag) }

// builtinTools are the tools Claude Code has of its own (files, shell, web). The chat must have none of them; this list is
// only used with a Claude Code too old to have the --tools flag that removes them all at once.
const builtinTools = "Bash,BashOutput,KillShell,KillBash,Edit,MultiEdit,Write,NotebookEdit,Read,Glob,Grep,LS,WebFetch,WebSearch,Task,TodoWrite,ExitPlanMode,SlashCommand"

// chatArgs builds the command line for the chat's Claude to suit the installed version. help is what `claude --help`
// printed; when it could not be read (empty) the current flags are assumed. Flags that only tidy things up are left out
// when the version lacks them. The one thing it will not do is start a Claude that cannot be kept to DiskScout's tools.
func chatArgs(help, cfgPath string, promptArgs []string) ([]string, error) {
	has := func(flag string) bool { return help == "" || strings.Contains(help, flag) }
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--mcp-config", cfgPath}
	if has("--strict-mcp-config") {
		args = append(args, "--strict-mcp-config")
	}
	switch {
	case has("--tools"):
		args = append(args, "--tools", "")
	case has("--disallowedTools") || has("--disallowed-tools"):
		args = append(args, "--disallowedTools", builtinTools) // an older Claude Code: name the built-in tools instead
	default:
		return nil, errors.New("this Claude Code is too old to be limited to DiskScout's own tools, so the chat was not started. Update it (run claude update, or reinstall it) and try again")
	}
	args = append(args, "--allowedTools", "mcp__"+mcpName)
	args = append(args, promptArgs...)
	if has("--no-session-persistence") {
		args = append(args, "--no-session-persistence")
	}
	if has("--setting-sources") {
		args = append(args, "--setting-sources", "project")
	}
	return args, nil
}

type ChatMsg struct {
	ID   int    `json:"id"`
	Role string `json:"role"` // user | assistant | tool | error | system
	Text string `json:"text"`
	Tool string `json:"tool,omitempty"`
	At   int64  `json:"at"`
}

type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.b = append(t.b, p...)
	if len(t.b) > 3000 {
		t.b = t.b[len(t.b)-3000:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

type Chat struct {
	a       *App
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	busy    bool
	msgs    []ChatMsg
	next    int
	dir     string
	errTail *tailBuffer
	fresh   bool   // a process was just started and has been told nothing yet
	sent    string // what the last message handed to Claude held (the conversation so far, if it was restarted), for tests
}

// A Claude process only remembers what it was told. It ends when the user presses Stop (or if it dies), and the next
// message starts a new one, while the panel still shows the whole conversation. So a new process is told the
// conversation so far along with the new message, or "continue" would mean nothing to it.
const replayIntro = "[DiskScout: this chat's Claude was restarted (the user pressed Stop, or it ended on its own), so you have lost your memory of the conversation so far. It is below, as the user saw it. Carry on from it: if their new message is short, such as \"continue\" or \"ok\", it refers to what you were last doing, so do that and do not start a new review. A tool call that has no result below was interrupted; the results of earlier calls are not shown, so call a tool again if you need one.]\n\n"

const maxReplay = 24000 // characters of earlier conversation to hand over: the most recent part if it is longer

// replayText turns the conversation so far into text for a new Claude process. It is "" when nothing was said yet.
func replayText(msgs []ChatMsg) string {
	var lines []string
	said := false
	for _, m := range msgs {
		switch m.Role {
		case "user":
			lines, said = append(lines, "User: "+m.Text), true
		case "assistant":
			lines, said = append(lines, "You: "+m.Text), true
		case "tool":
			lines = append(lines, "(You called "+m.Tool+" "+m.Text+")")
		case "system":
			if strings.HasPrefix(m.Text, "Stopped") {
				lines = append(lines, "[The user pressed Stop.]")
			}
		case "error":
			lines = append(lines, "[An error was shown to the user: "+shorten(m.Text, 300)+"]")
		}
	}
	if !said {
		return ""
	}
	var keep []string
	size, cut := 0, false
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if len(l) > maxReplay {
			l = l[:maxReplay] + "…"
		}
		if size+len(l) > maxReplay {
			cut = true
			break
		}
		size += len(l) + 2
		keep = append(keep, l)
	}
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	out := strings.Join(keep, "\n\n")
	if cut {
		out = "(Earlier messages are left out.)\n\n" + out
	}
	return out
}

func newChat(a *App) *Chat { return &Chat{a: a} }

func (c *Chat) state() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"available": findCLI() != "", "running": c.cmd != nil, "busy": c.busy}
}

func (c *Chat) broadcastState() { c.a.hub.Broadcast("chatstate", c.state()) }

func (c *Chat) add(role, text, tool string) {
	c.mu.Lock()
	c.next++
	m := ChatMsg{ID: c.next, Role: role, Text: text, Tool: tool, At: time.Now().UnixMilli()}
	c.msgs = append(c.msgs, m)
	if len(c.msgs) > 400 {
		c.msgs = c.msgs[len(c.msgs)-400:]
	}
	c.mu.Unlock()
	c.a.hub.Broadcast("chat", m)
}

func (c *Chat) History() []ChatMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ChatMsg{}, c.msgs...)
}

func chatEnv() []string {
	var out []string
	for _, e := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(e, "=", 2)[0])
		// a DiskScout that was itself started from a Claude Code session must not make the child think it is nested
		if k == "CLAUDECODE" || k == "CLAUDE_CODE_ENTRYPOINT" || k == "CLAUDE_CODE_SSE_PORT" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (c *Chat) ensure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		return nil
	}
	cli := findCLI()
	if cli == "" {
		return errors.New("Claude Code was not found on this computer, so the chat cannot start. Install it, or use Claude Desktop through \"Connect Claude\" instead")
	}
	if c.a.port == 0 {
		return errors.New("the app's address is not known yet")
	}
	if c.dir == "" {
		c.dir, _ = os.MkdirTemp("", "diskscout-chat-")
	}
	cfg := map[string]any{"mcpServers": map[string]any{mcpName: map[string]any{
		"type": "http", "url": "http://127.0.0.1:" + strconv.Itoa(c.a.port) + "/mcp", "headers": map[string]string{"X-Token": c.a.token}}}}
	cfgPath := filepath.Join(c.dir, "mcp.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return err
	}
	promptArgs := []string{"--append-system-prompt", chatPromptInline}
	if cliSupports(cli, "--append-system-prompt-file") {
		promptPath := filepath.Join(c.dir, "prompt.txt")
		if os.WriteFile(promptPath, []byte(chatPrompt), 0o600) == nil {
			promptArgs = []string{"--append-system-prompt-file", promptPath}
		}
	}
	args, err := chatArgs(cliHelp(cli), cfgPath, promptArgs)
	if err != nil {
		return err
	}
	cmd := exec.Command(cli, args...)
	cmd.Dir = c.dir // an empty folder: no project settings or CLAUDE.md to pick up
	cmd.Env = chatEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW: no console flashes up
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	tail := &tailBuffer{}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd, c.stdin, c.errTail = cmd, stdin, tail
	c.fresh = true
	go c.read(stdout)
	go func() {
		cmd.Wait()
		c.mu.Lock()
		current := c.cmd == cmd
		wasBusy := c.busy
		if current {
			c.cmd, c.stdin, c.busy = nil, nil, false
		}
		c.mu.Unlock()
		if current {
			if wasBusy {
				msg := "Claude stopped unexpectedly."
				if t := tail.String(); t != "" {
					msg += " " + t
					if strings.Contains(strings.ToLower(t), "unknown option") {
						helpCache.Delete(cli) // ask again next time: it may have been updated
						msg += "\n\nThis Claude Code does not know a flag DiskScout used, so it is older than DiskScout expects. Update it (run claude update, or reinstall it) and try again."
					}
				}
				c.add("error", msg, "")
			}
			c.broadcastState()
		}
	}()
	return nil
}

// Send hands the user's message to Claude. The answer arrives as chat events.
func (c *Chat) Send(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("write a message first")
	}
	c.mu.Lock()
	busy := c.busy
	c.mu.Unlock()
	if busy {
		return errors.New("Claude is still answering: wait, or press Stop")
	}
	if err := c.ensure(); err != nil {
		return err
	}
	// a process that has only just started knows nothing, so it is told what was said before (the panel shows only the
	// user's own words)
	payload := text
	c.mu.Lock()
	fresh := c.fresh
	c.fresh = false
	c.mu.Unlock()
	if fresh {
		if before := replayText(c.History()); before != "" {
			payload = replayIntro + before + "\n\nThe user's new message:\n" + text
		}
	}
	c.add("user", text, "")
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{
		"role": "user", "content": []map[string]any{{"type": "text", "text": payload}}}})
	c.mu.Lock()
	c.busy = true
	c.sent = payload
	stdin := c.stdin
	c.mu.Unlock()
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		c.mu.Lock()
		c.busy = false
		c.mu.Unlock()
		return err
	}
	c.broadcastState()
	return nil
}

func (c *Chat) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			return
		}
	}
}

func shorten(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c *Chat) handleLine(line []byte) {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "assistant":
		var blocks []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			var s string
			if json.Unmarshal(ev.Message.Content, &s) == nil && strings.TrimSpace(s) != "" {
				c.addClaudeText(s)
			}
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					c.addClaudeText(b.Text)
				}
			case "tool_use":
				c.add("tool", shorten(string(b.Input), 160), strings.TrimPrefix(b.Name, "mcp__"+mcpName+"__"))
			}
		}
	case "result":
		c.mu.Lock()
		c.busy = false
		c.mu.Unlock()
		if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
			msg := ev.Result
			if msg == "" {
				msg = "Claude stopped (" + ev.Subtype + ")."
			}
			c.addError(msg)
		}
		c.broadcastState()
	}
}

func (c *Chat) kill() {
	c.mu.Lock()
	cmd := c.cmd
	c.cmd, c.stdin, c.busy = nil, nil, false
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}

// Stop interrupts Claude; the next message starts a fresh process, which is told the conversation so far (see replayText).
func (c *Chat) Stop() {
	c.kill()
	c.add("system", "Stopped.", "")
	c.broadcastState()
}

// Reset forgets the conversation.
func (c *Chat) Reset() {
	c.kill()
	c.mu.Lock()
	c.msgs = nil
	c.mu.Unlock()
	c.a.hub.Broadcast("chatreset", map[string]any{})
	c.broadcastState()
}

func (c *Chat) Close() {
	c.kill()
	c.mu.Lock()
	dir := c.dir
	c.mu.Unlock()
	if dir != "" {
		os.RemoveAll(dir)
	}
}

// Claude Code reports a failed API call as ordinary-looking text ("API Error: 404 {...}") and then again as the result, and
// repeats it for each background call. It reaches the user once, as an error, with what it means.
func (c *Chat) addClaudeText(text string) {
	if strings.HasPrefix(strings.TrimSpace(text), "API Error:") {
		c.addError(text)
		return
	}
	c.add("assistant", text, "")
}

func (c *Chat) addError(text string) {
	text = explainChatError(text)
	c.mu.Lock()
	dup := len(c.msgs) > 0 && c.msgs[len(c.msgs)-1].Role == "error" && c.msgs[len(c.msgs)-1].Text == text
	c.mu.Unlock()
	if !dup {
		c.add("error", text, "")
	}
}

var modelGone = regexp.MustCompile(`not_found_error[^}]*model:\s*([A-Za-z0-9._:-]+)`)

// explainChatError says what a Claude Code error means for the person reading it, when we know.
func explainChatError(msg string) string {
	if m := modelGone.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("Claude Code asked for the model %s, which no longer exists. That means this computer has an old version of Claude Code. "+
			"Update it (open a terminal and run: claude update, or reinstall it) and try again.\n\n"+
			"If you cannot update right now, set the environment variable ANTHROPIC_SMALL_FAST_MODEL to a current Haiku model "+
			"(for example claude-haiku-5-5) and restart DiskScout.\n\n(%s)", m[1], shorten(strings.TrimSpace(msg), 160))
	}
	if low := strings.ToLower(msg); strings.Contains(low, "invalid api key") || strings.Contains(low, "please run /login") || strings.Contains(low, "not logged in") {
		return msg + "\n\nClaude Code is not signed in on this computer. Open a terminal, run claude, and sign in with /login."
	}
	return msg
}
