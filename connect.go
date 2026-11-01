package main

// Finding Claude on this computer and connecting DiskScout to it. Both Claude Desktop and the Claude Code CLI speak
// MCP, so one thing serves both: `DiskScout.exe --mcp`, a small bridge that they start themselves and that talks to
// the running DiskScout window. "Connect" only writes that one entry into the product's own config.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const mcpName = "diskscout"

type ConnectTarget struct {
	Found     bool   `json:"found"`     // the product is installed (or its config folder exists)
	Connected bool   `json:"connected"` // its config already has the diskscout entry
	Stale     bool   `json:"stale"`     // ...but it points at a different copy of the program
	Where     string `json:"where"`     // the config file (Desktop) or the CLI program
	Version   string `json:"version,omitempty"`
}

type ClaudeInfo struct {
	Exe     string        `json:"exe"`
	CLI     ConnectTarget `json:"cli"`
	Desktop ConnectTarget `json:"desktop"`
	Command string        `json:"command"` // what to type for the CLI by hand
	Snippet string        `json:"snippet"` // what to paste into Desktop's config by hand
}

func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "DiskScout.exe"
	}
	return exe
}

// findCLI locates the Claude Code command-line program.
func findCLI() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home := os.Getenv("USERPROFILE")
	for _, p := range []string{
		filepath.Join(home, `.local\bin\claude.exe`),
		filepath.Join(home, `.claude\local\claude.exe`),
		filepath.Join(os.Getenv("APPDATA"), `npm\claude.cmd`),
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Programs\claude\claude.exe`),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// desktopConfigs lists where Claude Desktop keeps claude_desktop_config.json: the normal installer uses %APPDATA%,
// the Microsoft Store/MSIX package redirects it into the package folder.
func desktopConfigs() []string {
	var out []string
	if m, _ := filepath.Glob(filepath.Join(os.Getenv("LOCALAPPDATA"), `Packages\*Claude*\LocalCache\Roaming\Claude`)); len(m) > 0 {
		for _, d := range m {
			out = append(out, filepath.Join(d, "claude_desktop_config.json"))
		}
	}
	out = append(out, filepath.Join(os.Getenv("APPDATA"), `Claude\claude_desktop_config.json`))
	return out
}

func readJSONMap(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// entryOf returns the diskscout entry of an mcpServers map, and whether it runs this very program.
func entryOf(cfg map[string]any, exe string) (present, current bool) {
	servers, _ := cfg["mcpServers"].(map[string]any)
	e, ok := servers[mcpName].(map[string]any)
	if !ok {
		return false, false
	}
	cmd, _ := e["command"].(string)
	return true, strings.EqualFold(filepath.Clean(cmd), filepath.Clean(exe))
}

func claudeInfo() ClaudeInfo {
	exe := selfExe()
	info := ClaudeInfo{Exe: exe, Command: fmt.Sprintf(`claude mcp add --scope user %s -- "%s" --mcp`, mcpName, exe)}
	snippet, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{mcpName: map[string]any{"command": exe, "args": []string{"--mcp"}}}}, "", "  ")
	info.Snippet = string(snippet)

	if cli := findCLI(); cli != "" {
		info.CLI = ConnectTarget{Found: true, Where: cli}
		if cfg, err := readJSONMap(filepath.Join(os.Getenv("USERPROFILE"), ".claude.json")); err == nil {
			info.CLI.Connected, _ = entryOf(cfg, exe)
			if present, current := entryOf(cfg, exe); present && !current {
				info.CLI.Stale = true
			}
		}
	}
	for _, p := range desktopConfigs() {
		_, dirErr := os.Stat(filepath.Dir(p))
		cfg, err := readJSONMap(p)
		if err != nil && dirErr != nil {
			continue
		}
		info.Desktop.Found = true
		if info.Desktop.Where == "" || err == nil {
			info.Desktop.Where = p
		}
		if err == nil {
			if present, current := entryOf(cfg, exe); present {
				info.Desktop.Connected, info.Desktop.Stale = true, !current
				info.Desktop.Where = p
				break
			}
		}
	}
	return info
}

// connectDesktop adds the entry to Claude Desktop's config, keeping everything else in it, after saving a copy.
func connectDesktop(path string) error {
	if path == "" {
		path = claudeInfo().Desktop.Where
	}
	if path == "" {
		return errors.New("Claude Desktop does not seem to be installed")
	}
	cfg, err := readJSONMap(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Claude Desktop's config is not valid JSON, so I left it alone: %w", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	if b, err := os.ReadFile(path); err == nil {
		bak := path + ".diskscout-backup"
		if _, e := os.Stat(bak); e != nil {
			os.WriteFile(bak, b, 0o644) // once: the user's own version from before we touched it
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[mcpName] = map[string]any{"command": selfExe(), "args": []string{"--mcp"}}
	cfg["mcpServers"] = servers
	out, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func disconnectDesktop(path string) error {
	cfg, err := readJSONMap(path)
	if err != nil {
		return err
	}
	if servers, ok := cfg["mcpServers"].(map[string]any); ok {
		delete(servers, mcpName)
		out, _ := json.MarshalIndent(cfg, "", "  ")
		return os.WriteFile(path, out, 0o644)
	}
	return nil
}

func runCLI(args ...string) (string, error) {
	cli := findCLI()
	if cli == "" {
		return "", errors.New("the claude command was not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Dir = os.TempDir()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func connectCLI() (string, error) {
	runCLI("mcp", "remove", mcpName, "--scope", "user") // replace a stale entry; "not found" is fine
	return runCLI("mcp", "add", "--scope", "user", mcpName, "--", selfExe(), "--mcp")
}

func disconnectCLI() (string, error) { return runCLI("mcp", "remove", mcpName, "--scope", "user") }
