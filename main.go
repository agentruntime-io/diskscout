// DiskScout: see where the space went (a treemap that fills in live while it scans) and find duplicate files by
// content. One portable .exe, no installer, no dependencies; the window is an Edge/Chrome "app" window pointed at a
// server that lives inside this same process.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	port := flag.Int("port", 0, "port to listen on (default: any free port)")
	noWindow := flag.Bool("no-window", false, "serve only: print the address instead of opening a window")
	inBrowser := flag.Bool("browser", false, "open in the default browser instead of an app window")
	importPath := flag.String("import", "", "open a duplicates.json report (or its folder) at start")
	dataFlag := flag.String("data", "", "folder for settings, hash cache and logs (default: next to the program)")
	idle := flag.Duration("idle-exit", 8*time.Second, "quit this long after the window closes (0 = never)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.StringVar(&uiDir, "ui-dir", "", "developer: serve the window's files from this folder instead of the built-in copy")
	mcpMode := flag.Bool("mcp", false, "run as an MCP server on stdin/stdout, for Claude Desktop or the Claude Code CLI")
	flag.Parse()
	if *showVersion {
		fmt.Println("DiskScout", version)
		return
	}

	dataDir := pickDataDir(*dataFlag)
	if *mcpMode {
		runMCPStdio(dataDir)
		return
	}
	logf, _ := openLog(filepath.Join(dataDir, "diskscout.log"))
	if logf != nil {
		defer logf.Close()
		log.SetOutput(logf)
	}
	log.Printf("DiskScout %s starting, data in %s", version, dataDir)
	lowerPriority()

	tok := make([]byte, 16)
	rand.Read(tok)
	app := newApp(dataDir, hex.EncodeToString(tok))

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil && *port != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // that port is busy: take any other
	}
	if err != nil {
		log.Fatalf("cannot listen: %v", err)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	app.port = actual
	writeConnection(dataDir, actual, app.token)
	defer os.Remove(connectionPath(dataDir))

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	srv := newServer(app, actual, stop)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("server: %v", err)
			stop()
		}
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", actual, app.token)
	log.Printf("listening on %s", ln.Addr())

	if *importPath != "" {
		if n, err := app.Import(*importPath); err != nil {
			log.Printf("import: %v", err)
		} else {
			log.Printf("imported %d groups from %s", n, *importPath)
		}
	}

	var profile string
	started := time.Now()
	switch {
	case *noWindow:
		fmt.Println(url)
	case *inBrowser:
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		cmd, prof, err := openAppWindow(url)
		profile = prof
		if err != nil {
			log.Printf("no app window: %v; using the default browser", err)
			exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		} else {
			go func() {
				cmd.Wait()
				if time.Since(started) > 5*time.Second { // a quick exit means it handed over to a running browser
					log.Printf("window closed")
					stop()
				}
			}()
		}
	}

	go func() { // free space is what this app is about: keep it fresh in the window
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if app.hub.count() > 0 {
					app.hub.Broadcast("disk", diskStats())
				}
			}
		}
	}()
	if *idle > 0 && !*noWindow {
		go func() { // belt and braces: quit if the window is gone without the process telling us
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if d, ok := app.hub.idleFor(); ok && d > *idle {
						log.Printf("no window for %s, quitting", d.Round(time.Second))
						stop()
					} else if !ok && !app.hub.wasConnected() && time.Since(started) > 2*time.Minute {
						log.Printf("no window ever connected, quitting")
						stop()
					}
				}
			}
		}()
	}

	<-ctx.Done()
	app.Cancel()
	app.chat.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	hs.Shutdown(shutdown)
	cancel()
	if profile != "" {
		for i := 0; i < 10; i++ { // the browser may still be letting go of its files
			if os.RemoveAll(profile) == nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	log.Printf("bye")
}

func pickDataDir(flagVal string) string {
	try := func(dir string) bool {
		if os.MkdirAll(dir, 0o755) != nil {
			return false
		}
		f, err := os.CreateTemp(dir, ".w")
		if err != nil {
			return false
		}
		f.Close()
		os.Remove(f.Name())
		return true
	}
	if flagVal != "" && try(flagVal) {
		return flagVal
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Join(filepath.Dir(exe), "diskscout-data"); try(dir) { // portable: data travels with the program
			return dir
		}
	}
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "DiskScout")
	try(dir)
	return dir
}

func openLog(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > 1<<20 {
		os.Remove(path)
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// openAppWindow opens url in a chromeless Edge (or Chrome) window with its own throwaway profile, so it neither
// touches the user's browser nor shows tabs or an address bar.
func openAppWindow(url string) (*exec.Cmd, string, error) {
	var candidates []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if base := os.Getenv(env); base != "" {
			candidates = append(candidates,
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
		}
	}
	profile, err := os.MkdirTemp("", "diskscout-window-")
	if err != nil {
		return nil, "", err
	}
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		cmd := exec.Command(exe, "--app="+url, "--user-data-dir="+profile, "--window-size=1360,900",
			"--no-first-run", "--no-default-browser-check", "--disable-extensions", "--disable-sync",
			"--disable-background-networking", "--disable-features=Translate")
		if err := cmd.Start(); err == nil {
			return cmd, profile, nil
		}
	}
	os.RemoveAll(profile)
	return nil, "", fmt.Errorf("neither Edge nor Chrome was found")
}
