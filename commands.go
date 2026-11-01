package main

// Letting Claude run command-line programs, without giving it a shell.
//
// There are three kinds of command, and the kind decides what happens:
//   read     a known diagnostic that only looks (docker system df, wsl -l -v, go env, powercfg /a ...): runs at once
//   cleanup  a known command that frees space by deleting something (docker system prune, go clean, npm cache clean ...):
//            waits in the window until the user approves the exact command line
//   other    anything else: waits for approval too, with a warning that it could do anything
//
// Commands are started directly, never through cmd.exe or PowerShell, so they cannot be chained with && or |. Output is
// capped, the run has a time limit, and every command is appended to commands.log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
)

type CommandResult struct {
	Command   string  `json:"command"`
	Class     string  `json:"class"`
	ExitCode  int     `json:"exit_code"`
	Output    string  `json:"output"`
	Truncated bool    `json:"truncated,omitempty"`
	Seconds   float64 `json:"seconds"`
	Approved  bool    `json:"approved_by_user"`
}

var maxCommandOutput = 64 << 10

type cmdRule struct {
	prog string
	head []string       // the leading arguments, which must match exactly (case does not matter)
	pos  *regexp.Regexp // positional arguments allowed after them (nil: none), e.g. variable names for `go env`
	deny []string       // flags that turn a read into a change
	note string
}

var (
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	winSwitch = regexp.MustCompile(`^/[A-Za-z?]+(=[A-Za-z0-9:\\._-]+)?$`) // /for=C: and the like

	readRules = []cmdRule{
		{prog: "docker", head: []string{"system", "df"}},
		{prog: "docker", head: []string{"images"}},
		{prog: "docker", head: []string{"image", "ls"}},
		{prog: "docker", head: []string{"ps"}},
		{prog: "docker", head: []string{"container", "ls"}},
		{prog: "docker", head: []string{"volume", "ls"}},
		{prog: "docker", head: []string{"builder", "du"}},
		{prog: "docker", head: []string{"info"}},
		{prog: "docker", head: []string{"version"}},
		{prog: "wsl", head: []string{"-l"}},
		{prog: "wsl", head: []string{"--list"}},
		{prog: "wsl", head: []string{"--status"}},
		{prog: "wsl", head: []string{"--version"}},
		{prog: "powercfg", head: []string{"/a"}},
		{prog: "powercfg", head: []string{"/availablesleepstates"}},
		{prog: "vssadmin", head: []string{"list", "shadowstorage"}},
		{prog: "vssadmin", head: []string{"list", "shadows"}},
		{prog: "reagentc", head: []string{"/info"}},
		{prog: "go", head: []string{"env"}, pos: envName, deny: []string{"-w", "-u"}},
		{prog: "go", head: []string{"version"}},
		{prog: "npm", head: []string{"config", "get", "cache"}},
		{prog: "uv", head: []string{"cache", "dir"}},
		{prog: "pip", head: []string{"cache", "dir"}},
		{prog: "pip", head: []string{"cache", "info"}},
	}

	cleanupRules = []cmdRule{
		{prog: "docker", head: []string{"system", "prune"}, note: "Removes stopped containers, unused networks and dangling images (and ALL unused images with -a). Volumes, which hold databases, are only removed if --volumes is given."},
		{prog: "docker", head: []string{"image", "prune"}, note: "Removes unused Docker images (all unused ones with -a)."},
		{prog: "docker", head: []string{"container", "prune"}, note: "Removes stopped Docker containers."},
		{prog: "docker", head: []string{"builder", "prune"}, note: "Removes Docker's build cache. It rebuilds on the next build."},
		{prog: "docker", head: []string{"volume", "prune"}, note: "Removes Docker volumes that no container uses. Volumes hold data such as databases: this can lose data."},
		{prog: "docker", head: []string{"network", "prune"}, note: "Removes unused Docker networks."},
		{prog: "go", head: []string{"clean"}, note: "Removes Go build or module caches. They rebuild on demand."},
		{prog: "npm", head: []string{"cache", "clean"}, note: "Empties npm's download cache."},
		{prog: "uv", head: []string{"cache", "clean"}, note: "Empties uv's cache. The files are re-downloaded when needed."},
		{prog: "pip", head: []string{"cache", "purge"}, note: "Empties pip's cache."},
		{prog: "powercfg", head: []string{"/hibernate"}, note: "Changes hibernation. 'off' deletes hiberfil.sys and turns off hibernate and Fast Startup. Needs administrator rights."},
		{prog: "powercfg", head: []string{"/h"}, note: "Changes hibernation. 'off' deletes hiberfil.sys and turns off hibernate and Fast Startup. Needs administrator rights."},
		{prog: "wsl", head: []string{"--shutdown"}, note: "Stops every WSL distribution and Docker Desktop's engine. Anything running in them is interrupted."},
		{prog: "wsl", head: []string{"--manage"}, note: "Changes a WSL distribution (for example making its disk sparse, so it shrinks)."},
	}
)

// progName is a program's name as the rules see it: no folder, no extension, lower case.
func progName(p string) string {
	b := strings.ToLower(filepath.Base(p))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}

func (r cmdRule) matches(prog string, args []string) bool {
	if progName(prog) != r.prog || len(args) < len(r.head) {
		return false
	}
	for i, h := range r.head {
		if !strings.EqualFold(args[i], h) {
			return false
		}
	}
	return true
}

// restIsHarmless reports whether the arguments after a read rule's head only add flags (and values for the flags that
// take one), plus the positional names the rule allows. A read-only command stays read-only only if nothing else rides along.
func (r cmdRule) restIsHarmless(rest []string) bool {
	valueNext := false
	for _, a := range rest {
		switch {
		case valueNext:
			valueNext = false
		case strings.HasPrefix(a, "-") || winSwitch.MatchString(a):
			for _, d := range r.deny {
				if strings.EqualFold(a, d) {
					return false
				}
			}
			if a == "--format" || a == "--filter" || a == "-f" {
				valueNext = true // the next argument is that flag's value
			}
		case r.pos != nil && r.pos.MatchString(a):
		default:
			return false
		}
	}
	return true
}

// classifyCommand decides what kind of command this is, and returns a note explaining a cleanup command.
func classifyCommand(program string, args []string) (class, note string) {
	for _, r := range readRules {
		if r.matches(program, args) && r.restIsHarmless(args[len(r.head):]) {
			return "read", ""
		}
	}
	for _, r := range cleanupRules {
		if r.matches(program, args) {
			return "cleanup", r.note
		}
	}
	return "other", "This is not one of the known diagnostic or cleanup commands, so it could do anything. Read it carefully before you approve it."
}

func commandLine(program string, args []string) string {
	parts := []string{quoteArg(program)}
	for _, a := range args {
		parts = append(parts, quoteArg(a))
	}
	return strings.Join(parts, " ")
}

func quoteArg(a string) string {
	if a == "" || strings.ContainsAny(a, " \t\"") {
		return `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	return a
}

// decodeOutput turns a program's raw output into text. Some Windows tools (wsl.exe) print UTF-16.
func decodeOutput(b []byte) string {
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || bytes.Count(b, []byte{0}) > len(b)/4) {
		if b[0] == 0xFF && b[1] == 0xFE {
			b = b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return strings.ReplaceAll(string(utf16.Decode(u)), "\r\n", "\n")
	}
	return strings.ReplaceAll(strings.ToValidUTF8(string(b), "?"), "\r\n", "\n")
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := l.max - l.buf.Len()
	if room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
			l.truncated = true
		} else {
			l.buf.Write(p)
		}
	} else if len(p) > 0 {
		l.truncated = true
	}
	return len(p), nil
}

// runProgram starts the program directly (no shell) and collects its output.
func runProgram(ctx context.Context, path string, args []string, timeout time.Duration) CommandResult {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out := &limitedBuffer{max: maxCommandOutput}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	res := CommandResult{Seconds: float64(int(time.Since(start).Seconds()*10)) / 10}
	text := decodeOutput(out.buf.Bytes())
	switch {
	case err == nil:
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		res.ExitCode = -1
		text += fmt.Sprintf("\n[stopped: it ran longer than %s]", timeout)
	case errors.As(err, new(*exec.ExitError)):
		res.ExitCode = err.(*exec.ExitError).ExitCode()
	default:
		res.ExitCode = -1
		text += "\n[could not run: " + err.Error() + "]"
	}
	res.Output, res.Truncated = strings.TrimSpace(text), out.truncated
	return res
}

// RunCommand classifies the command, asks the user when it is not a plain diagnostic, then runs it.
func (a *App) RunCommand(ctx context.Context, actor, program string, args []string, reason string, timeout time.Duration) (*CommandResult, error) {
	program = strings.TrimSpace(program)
	if program == "" {
		return nil, errors.New("program is empty")
	}
	for _, s := range append([]string{program}, args...) {
		if strings.ContainsRune(s, 0) {
			return nil, errors.New("an argument contains an invalid character")
		}
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return nil, fmt.Errorf("%q was not found on this computer's PATH", program)
	}
	class, note := classifyCommand(program, args)
	line := commandLine(program, args)
	approved := false
	if class != "read" {
		if a.hub.count() == 0 {
			return nil, errors.New("no DiskScout window is open, so nobody can approve this command. Ask the user to open the DiskScout window")
		}
		ap := &Approval{Actor: actor, Kind: "command", Reason: reason, Command: line, Class: class, Note: note,
			Items: []ApprovalItem{{Path: path, Name: line, Kind: "command"}}}
		d, err := a.ask(ctx, ap)
		if err != nil {
			return nil, err
		}
		if !d.approved {
			return nil, errors.New("the user declined to run this command")
		}
		approved = true
	}
	res := runProgram(ctx, path, args, timeout)
	res.Command, res.Class, res.Approved = line, class, approved
	rec, _ := json.Marshal(map[string]any{"time": time.Now().Format(time.RFC3339), "by": actor, "class": class, "command": line,
		"approved": approved, "exit": res.ExitCode, "seconds": res.Seconds})
	if f, err := os.OpenFile(filepath.Join(a.dataDir, "commands.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Write(append(rec, '\n'))
		f.Close()
	}
	if class == "read" {
		a.hub.Broadcast("toast", map[string]string{"text": actor + " ran: " + line})
	}
	return &res, nil
}
