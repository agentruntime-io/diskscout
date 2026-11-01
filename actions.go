package main

// Everything that changes the disk goes through here, behind the same guard rails:
//   - only files the app listed (a duplicate group, or the space map) can be touched
//   - a duplicate is deleted only while it is still exactly what the scan saw (size and modified time)
//   - and, unless the caller opts out, only after both it and a surviving copy are read again in full and found
//     byte-for-byte identical, so a copy that has since gone bad (or become unreadable) is never the one left behind
//   - the last remaining copy of a file is never removed unless the user ticks the box that says so
//   - folders from the map go to the Recycle Bin only, and system locations are off limits
//   - every deletion is appended to deleted.log

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// trashFile is the one call that sends something to the Recycle Bin (a variable so tests can swap it).
var trashFile = moveToRecycleBin

type DelResult struct {
	Path  string `json:"path"`
	OK    bool   `json:"ok"`
	Size  int64  `json:"size,omitempty"`
	Error string `json:"error,omitempty"`
	Gone  bool   `json:"gone,omitempty"`
	// Verified is the copy this one was proven identical to, byte for byte, just before it was removed.
	Verified string `json:"verified,omitempty"`
}

// problem returns why a copy can't be deleted right now, or "" if it is still exactly what the scan saw.
func (a *App) problem(c *Copy, size int64) string {
	a.mu.RLock()
	gone := c.Gone
	a.mu.RUnlock()
	if gone {
		return "already deleted"
	}
	st, err := os.Lstat(c.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return "already gone"
		}
		return err.Error()
	}
	if fileAttrs(st)&skipAttrs != 0 || !st.Mode().IsRegular() {
		return "not a regular file any more"
	}
	if st.Size() != size || st.ModTime().UnixNano() != c.MTime {
		return "changed since the scan"
	}
	return ""
}

// digestFile reads a file again from the start and returns its SHA-256. It never uses a cached digest: the point is to
// see what is on the disk now. (A variable so tests can make a read fail.)
var digestFile = func(path string, buf []byte) (string, error) {
	return fullDigest(context.Background(), path, buf, func(int64) {})
}

func (a *App) audit(path string, size int64, mode, actor, against string) {
	rec := map[string]any{"time": time.Now().Format(time.RFC3339), "path": path, "size": size, "mode": mode}
	if actor != "" {
		rec["by"] = actor // set when Claude asked and the user approved
	}
	if against != "" {
		rec["identical_to"] = against // the copy it was byte-compared with just before removal
	}
	line, _ := json.Marshal(rec)
	if f, err := os.OpenFile(filepath.Join(a.dataDir, "deleted.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
}

// Delete removes duplicate copies. mode is "trash" (Recycle Bin) or "permanent". Each copy is first compared, byte for
// byte, with a copy that stays.
func (a *App) Delete(paths []string, mode string, allowAll bool) ([]DelResult, error) {
	return a.deleteAs("", paths, mode, allowAll, true)
}

// DeleteUnchecked is Delete without the byte-for-byte comparison, for when the user has chosen speed over it.
func (a *App) DeleteUnchecked(paths []string, mode string, allowAll bool) ([]DelResult, error) {
	return a.deleteAs("", paths, mode, allowAll, false)
}

func (a *App) deleteAs(actor string, paths []string, mode string, allowAll, verify bool) ([]DelResult, error) {
	var remove func(string) error
	switch mode {
	case "trash":
		remove = trashFile
	case "permanent":
		remove = os.Remove
	default:
		return nil, fmt.Errorf("unknown mode %q", mode)
	}
	a.delMu.Lock()
	defer a.delMu.Unlock()

	results := []DelResult{}
	byGroup := map[*Group][]*Copy{}
	var order []*Group
	seen := map[string]bool{}
	a.mu.RLock()
	for _, p := range paths {
		k := normKey(p)
		if seen[k] {
			continue
		}
		seen[k] = true
		r, ok := a.index[k]
		if !ok {
			results = append(results, DelResult{Path: p, Error: "not part of this report"})
			continue
		}
		if _, dup := byGroup[r.g]; !dup {
			order = append(order, r.g)
		}
		byGroup[r.g] = append(byGroup[r.g], r.c)
	}
	a.mu.RUnlock()

	type removed struct {
		g *Group
		c *Copy
	}
	var done []removed
	buf := make([]byte, 1<<20)
	for _, g := range order {
		targets := byGroup[g]
		doomed := map[*Copy]bool{}
		for _, c := range targets {
			doomed[c] = true
		}
		a.mu.RLock()
		copies := append([]*Copy(nil), g.Copies...)
		a.mu.RUnlock()
		var keepers []*Copy // copies that stay and are still on the disk as scanned, the suggested KEEP first
		for _, c := range copies {
			if !doomed[c] && a.problem(c, g.Size) == "" {
				keepers = append(keepers, c)
			}
		}
		sort.SliceStable(keepers, func(i, j int) bool { return keepers[i].Keep && !keepers[j].Keep })
		survivors := len(keepers)
		if survivors == 0 && !allowAll {
			for _, c := range targets {
				results = append(results, DelResult{Path: c.Path, Error: "would remove every remaining copy of this file"})
			}
			continue
		}
		// the copy that stays is read in full once per group, then each copy to remove is read and compared with it
		var proofPath, proofDigest, proofErr string
		proofTried := false
		check := func(c *Copy) (against, why string) {
			if !verify || len(keepers) == 0 {
				return "", "" // nothing to compare with: only the "I understand" path gets here
			}
			if !proofTried {
				proofTried = true
				for _, k := range keepers {
					d, err := digestFile(k.Path, buf)
					if err != nil {
						proofErr = err.Error()
						continue
					}
					proofPath, proofDigest = k.Path, d
					break
				}
			}
			if proofDigest == "" {
				return "", "the copy you are keeping could not be read (" + proofErr + "), so nothing from this group was removed"
			}
			d, err := digestFile(c.Path, buf)
			if err != nil {
				return "", "could not be read to check it against the copy you are keeping: " + err.Error()
			}
			if d != proofDigest {
				return "", "no longer matches the copy you are keeping"
			}
			return proofPath, ""
		}
		for _, c := range targets {
			if why := a.problem(c, g.Size); why != "" {
				results = append(results, DelResult{Path: c.Path, Error: why, Gone: strings.HasPrefix(why, "already")})
				continue
			}
			against, why := check(c)
			if why != "" {
				results = append(results, DelResult{Path: c.Path, Error: why})
				continue
			}
			if err := remove(c.Path); err != nil {
				results = append(results, DelResult{Path: c.Path, Error: err.Error()})
				continue
			}
			a.audit(c.Path, g.Size, mode, actor, against)
			results = append(results, DelResult{Path: c.Path, OK: true, Size: g.Size, Verified: against})
			done = append(done, removed{g, c})
		}
	}

	var freed int64
	a.mu.Lock()
	for _, d := range done {
		d.c.Gone = true
		freed += d.g.Size
	}
	for _, d := range done {
		a.touchGroupLocked(d.g)
	}
	t := a.tree
	a.mu.Unlock()
	if t != nil {
		for _, d := range done {
			t.forgetFile(d.c.Path, d.g.Size)
			if mode == "trash" {
				t.bumpRecycle(d.c.Path, d.g.Size, 1)
			}
		}
	}
	a.afterDelete(len(done), freed, mode)
	return results, nil
}

// touchGroupLocked records that a group changed, and promotes a new KEEP if the old one was deleted.
func (a *App) touchGroupLocked(g *Group) {
	a.rev++
	g.Rev = a.rev
	hasKeep := false
	for _, c := range g.Copies {
		if !c.Gone && c.Keep {
			hasKeep = true
		}
	}
	if !hasKeep {
		pickKeeper(g, a.settings.Prefer)
	}
}

func (a *App) afterDelete(n int, freed int64, mode string) {
	if n == 0 {
		return
	}
	a.treeRev.Add(1)
	a.hub.Broadcast("tree", map[string]any{"rev": a.treeRev.Load()})
	a.publishGroups()
	a.hub.Broadcast("deleted", map[string]any{"count": n, "bytes": freed, "mode": mode})
}

// Verify checks every listed copy against the disk and marks the ones that no longer exist, so the window reflects
// deletions made elsewhere (Explorer, another program).
func (a *App) Verify() int {
	type item struct {
		g *Group
		c *Copy
	}
	var all []item
	a.mu.RLock()
	for _, g := range a.groups {
		for _, c := range g.Copies {
			if !c.Gone {
				all = append(all, item{g, c})
			}
		}
	}
	a.mu.RUnlock()
	var missing []item
	for _, it := range all {
		if _, err := os.Lstat(it.c.Path); os.IsNotExist(err) {
			missing = append(missing, it)
		}
	}
	if len(missing) == 0 {
		return 0
	}
	a.mu.Lock()
	for _, it := range missing {
		it.c.Gone = true
	}
	for _, it := range missing {
		a.touchGroupLocked(it.g)
	}
	t := a.tree
	a.mu.Unlock()
	if t != nil {
		for _, it := range missing {
			t.forgetFile(it.c.Path, it.g.Size)
		}
	}
	a.afterDelete(0, 0, "")
	a.treeRev.Add(1)
	a.hub.Broadcast("tree", map[string]any{"rev": a.treeRev.Load()})
	a.publishGroups()
	return len(missing)
}

// protectedPath explains why a folder or file from the map must not be removed, or returns "".
func protectedPath(p string) string {
	p = filepath.Clean(p)
	if len(p) <= 3 {
		return "that is a whole drive"
	}
	low := strings.ToLower(p)
	profile := strings.ToLower(os.Getenv("USERPROFILE"))
	exact := []string{profile, filepath.Dir(profile), strings.ToLower(os.Getenv("ProgramData")),
		strings.ToLower(os.Getenv("ProgramFiles")), strings.ToLower(os.Getenv("ProgramFiles(x86)")), strings.ToLower(os.Getenv("SystemRoot"))}
	for _, e := range exact {
		if e != "" && low == strings.ToLower(filepath.Clean(e)) {
			return "that is a system or profile folder"
		}
	}
	if sr := strings.ToLower(os.Getenv("SystemRoot")); sr != "" && hasPathPrefix(low, sr) {
		return "that is inside Windows"
	}
	for _, seg := range strings.Split(low, `\`) {
		if seg == "$recycle.bin" || seg == "system volume information" {
			return "that is a system folder"
		}
	}
	return ""
}

// TrashPath sends a file or folder from the space map to the Recycle Bin.
func (a *App) TrashPath(path string) DelResult { return a.trashPathAs("", path) }

func (a *App) trashPathAs(actor, path string) DelResult {
	a.delMu.Lock()
	defer a.delMu.Unlock()
	path = filepath.Clean(path)
	fail := func(msg string) DelResult { return DelResult{Path: path, Error: msg} }
	if why := protectedPath(path); why != "" {
		return fail(why)
	}
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	if t == nil {
		return fail("there is no space map yet: run a scan first")
	}
	inside := false
	for _, r := range t.Roots {
		if hasPathPrefix(path, r) {
			inside = true
		}
	}
	if !inside {
		return fail("outside the scanned folders")
	}
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DelResult{Path: path, Error: "already gone", Gone: true}
		}
		return fail(err.Error())
	}
	if fileAttrs(st)&skipAttrs != 0 {
		return fail("that is a link or a cloud placeholder, not a real file or folder")
	}
	var size int64
	if st.IsDir() {
		n, exact := t.Root.find(path)
		if n == nil || !exact {
			return fail("that folder is not on the map (it was too small to keep), so its size is unknown")
		}
		size, _, _ = n.sums()
	} else {
		size = st.Size()
	}
	if err := trashFile(path); err != nil {
		return fail(err.Error())
	}
	a.audit(path, size, "trash", actor, "")
	if st.IsDir() {
		sz, cnt, _ := t.forgetDir(path)
		t.bumpRecycle(path, sz, cnt)
	} else {
		t.forgetFile(path, size)
		t.bumpRecycle(path, size, 1)
	}
	// duplicate copies inside what was removed are gone too (no map adjustment: the map already lost them)
	a.mu.Lock()
	touched := map[*Group]bool{}
	for _, r := range a.index {
		if !r.c.Gone && hasPathPrefix(r.c.Path, path) {
			r.c.Gone = true
			touched[r.g] = true
		}
	}
	for g := range touched {
		a.touchGroupLocked(g)
	}
	a.mu.Unlock()
	a.afterDelete(1, size, "trash")
	return DelResult{Path: path, OK: true, Size: size}
}

// Rescan reads one folder of the map again, so changes made outside the app show up without a full scan.
func (a *App) Rescan(path string) (int64, error) {
	a.smu.Lock()
	busy := a.running
	a.smu.Unlock()
	if busy {
		return 0, errBusy
	}
	a.mu.RLock()
	t, opts := a.tree, a.opts
	a.mu.RUnlock()
	if t == nil {
		return 0, fmt.Errorf("there is no space map yet")
	}
	ctx, cancel := contextWithTimeout(10 * time.Minute)
	defer cancel()
	delta, ok := t.rescan(ctx, filepath.Clean(path), opts)
	if !ok {
		return 0, fmt.Errorf("that folder is not on the map")
	}
	a.treeRev.Add(1)
	a.hub.Broadcast("tree", map[string]any{"rev": a.treeRev.Load()})
	return delta, nil
}

// Reveal shows a file in Explorer. The path must be one the app knows about.
func (a *App) Reveal(path string) error {
	path = filepath.Clean(path)
	known := false
	a.mu.RLock()
	if _, ok := a.index[normKey(path)]; ok {
		known = true
	}
	t := a.tree
	a.mu.RUnlock()
	if !known && t != nil {
		for _, r := range t.Roots {
			if hasPathPrefix(path, r) {
				known = true
			}
		}
	}
	if !known {
		return fmt.Errorf("not part of this scan")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	cmd := exec.Command("explorer.exe")
	if st.IsDir() {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe "` + path + `"`}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	}
	return cmd.Start() // explorer exits non-zero even when it worked, so never wait on it
}

// EmptyRecycleBin permanently empties one drive's Recycle Bin and refreshes its block on the map.
func (a *App) EmptyRecycleBin(drive string) (int64, error) {
	drive = strings.ToUpper(strings.TrimSpace(drive))
	if len(drive) == 2 && drive[1] == ':' {
		drive += `\`
	}
	if !isDriveRoot(drive) {
		return 0, fmt.Errorf("give a drive such as C:")
	}
	a.delMu.Lock()
	defer a.delMu.Unlock()
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	var before int64
	bin := drive + "$Recycle.Bin"
	if t != nil {
		if n, ok := t.Root.find(bin); ok && n != nil {
			before = n.stats().Total
		}
	}
	if err := emptyRecycleBin(drive); err != nil {
		return 0, err
	}
	a.audit(bin, before, "empty-recycle-bin", "", "")
	if t != nil {
		a.Rescan(bin) // best effort: if the bin is not on the map there is nothing to refresh
	}
	a.afterDelete(1, before, "empty-recycle-bin")
	return before, nil
}
