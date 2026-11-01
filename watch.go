package main

// Live updating: the map of the folder you have open follows the disk.
//
// Windows tells the app (ReadDirectoryChangesW) when something under a folder changes. After a quiet moment the app
// reads again the nearest mapped folder around each change and swaps it in, the same refresh the Rescan button does.
// Only the open folder is watched: a whole drive changes every second, and following it would mean reading it forever.
// To stay polite it waits for quiet, keeps a gap between refreshes that grows with how long the last one took, and
// switches itself off, saying so, if a refresh turns out to be heavy.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var pCancelIoEx = kernel32.NewProc("CancelIoEx")

const (
	liveQuiet    = 1500 * time.Millisecond // how long nothing may change before the folder is read again
	liveMinGap   = 4 * time.Second         // the least time between two refreshes
	liveTooHeavy = 45 * time.Second        // a refresh that takes this long means this folder cannot be followed live
	liveMaxJobs  = 6                       // more separate places than this changed: read the whole open folder instead
	notifyMask   = syscall.FILE_NOTIFY_CHANGE_FILE_NAME | syscall.FILE_NOTIFY_CHANGE_DIR_NAME | syscall.FILE_NOTIFY_CHANGE_SIZE |
		syscall.FILE_NOTIFY_CHANGE_LAST_WRITE | syscall.FILE_NOTIFY_CHANGE_CREATION
)

type liveState struct {
	mu     sync.Mutex
	path   string
	cancel context.CancelFunc
	done   chan struct{}
}

// SetLive starts following a folder, or (on=false) stops following whatever was followed. It follows one folder at a time.
func (a *App) SetLive(on bool, path string) error {
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	if a.live.cancel != nil {
		a.live.cancel()
		<-a.live.done
		a.live.cancel, a.live.done, a.live.path = nil, nil, ""
	}
	if !on || path == "" {
		return nil
	}
	path = filepath.Clean(path)
	if len(path) <= 3 { // C:\
		return errors.New("a whole drive changes too often to follow live: open a folder in it")
	}
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	if t == nil {
		return errors.New("there is no space map yet")
	}
	if n, exact := t.Root.find(path); n == nil || !exact || n == t.Root {
		return errors.New("that folder is not on the map")
	}
	h, err := openForWatch(path)
	if err != nil {
		return fmt.Errorf("Windows would not let the app watch that folder: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.live.cancel, a.live.done, a.live.path = cancel, done, path
	go func() {
		defer close(done)
		a.watchLoop(ctx, path, h)
	}()
	return nil
}

// pauseLive stops following a folder because of something that happened, and tells the window why.
func (a *App) pauseLive(path, why string) {
	a.live.mu.Lock()
	if a.live.path == path && a.live.cancel != nil {
		a.live.cancel() // the loop is the caller: it exits on its own, so this must not wait for it
	}
	a.live.mu.Unlock()
	a.hub.Broadcast("live", map[string]any{"on": false, "path": path, "reason": why})
}

func openForWatch(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// share everything, so the folder can still be renamed or deleted while it is watched
	return syscall.CreateFile(p, syscall.FILE_LIST_DIRECTORY, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// parseNotify reads the names (relative to the watched folder) out of a ReadDirectoryChangesW buffer.
func parseNotify(buf []byte) []string {
	var names []string
	for off := uint32(0); int(off)+12 <= len(buf); {
		info := (*syscall.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
		n := int(info.FileNameLength / 2)
		if int(off)+12+n*2 <= len(buf) {
			u := (*[1 << 15]uint16)(unsafe.Pointer(&info.FileName))[:n:n]
			names = append(names, syscall.UTF16ToString(u))
		}
		if info.NextEntryOffset == 0 {
			break
		}
		off += info.NextEntryOffset
	}
	return names
}

// liveIgnore says whether a change is the app's own doing (its logs and hash cache, the chat's scratch folder).
func (a *App) liveIgnore(full string) bool {
	return hasPathPrefix(full, a.dataDir) || strings.Contains(strings.ToLower(full), `\diskscout-chat-`) ||
		strings.Contains(strings.ToLower(full), `\system volume information`)
}

// liveTargets decides which mapped folders to read again: the nearest folder on the map around each change, with
// folders inside another target dropped. Many separate places, or an overflowed notification buffer, mean the open
// folder as a whole.
func (a *App) liveTargets(root string, changed map[string]string, overflow bool) []string {
	if overflow {
		return []string{root}
	}
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	if t == nil {
		return nil
	}
	set := map[string]string{}
	for _, d := range changed {
		p := d
		for {
			if !hasPathPrefix(p, root) {
				p = root
				break
			}
			if n, exact := t.Root.find(p); n != nil && exact && n != t.Root && n.Parent != nil {
				break
			}
			parent := filepath.Dir(p)
			if parent == p || len(parent) < len(root) {
				p = root
				break
			}
			p = parent
		}
		set[normKey(p)] = p
	}
	var all []string
	for _, p := range set {
		all = append(all, p)
	}
	sort.Slice(all, func(i, j int) bool { return len(all[i]) < len(all[j]) })
	var out []string
	for _, p := range all {
		inside := false
		for _, o := range out {
			if hasPathPrefix(p, o) {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, p)
		}
	}
	if len(out) > liveMaxJobs {
		return []string{root}
	}
	return out
}

func (a *App) watchLoop(ctx context.Context, root string, h syscall.Handle) {
	events := make(chan []string, 256)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(events)
		buf := make([]byte, 1<<16)
		for ctx.Err() == nil {
			var n uint32
			if err := syscall.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, notifyMask, &n, nil, 0); err != nil {
				return
			}
			names := []string{""} // "" means: so much changed that Windows lost the details
			if n > 0 {
				names = parseNotify(buf[:n])
			}
			select {
			case events <- names:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		for { // the reader is blocked inside Windows: cancel its wait (again if it had not started yet), then let go of the folder
			pCancelIoEx.Call(uintptr(h), 0)
			select {
			case <-readerDone:
				syscall.CloseHandle(h)
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()

	changed := map[string]string{}
	overflow := false
	var quietAt, notBefore time.Time
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case names, ok := <-events:
			if !ok {
				if ctx.Err() == nil {
					a.pauseLive(root, "Windows stopped reporting changes in that folder (it may have been renamed or removed), so live updating was turned off.")
				}
				return
			}
			for _, nm := range names {
				if nm == "" {
					overflow = true
					continue
				}
				full := filepath.Join(root, nm)
				if a.liveIgnore(full) {
					continue
				}
				d := filepath.Dir(full)
				changed[normKey(d)] = d
			}
			quietAt = time.Now().Add(liveQuiet)
		case <-tick.C:
			if (len(changed) == 0 && !overflow) || time.Now().Before(quietAt) || time.Now().Before(notBefore) {
				continue
			}
			targets := a.liveTargets(root, changed, overflow)
			changed, overflow = map[string]string{}, false
			start := time.Now()
			busy := false
			for _, tg := range targets {
				if ctx.Err() != nil {
					return
				}
				if _, err := a.Rescan(tg); errors.Is(err, errBusy) {
					busy = true
					changed[normKey(tg)] = tg // a scan is running: try again soon
				}
			}
			took := time.Since(start)
			if took > liveTooHeavy {
				a.pauseLive(root, "Reading that folder again took "+took.Round(time.Second).String()+", so live updating was turned off for it. Use Rescan when you want it refreshed, or open a smaller folder.")
				return
			}
			notBefore = time.Now().Add(max(liveMinGap, 3*took)) // the busier the folder, the longer the breather
			if busy {
				notBefore = time.Now().Add(5 * time.Second)
			}
		}
	}
}
