package main

// Opening a report written by the Python dupfinder (duplicates.json), so earlier results are usable at once
// without scanning again.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

type reportDoc struct {
	Generated string         `json:"generated"`
	Roots     []string       `json:"roots"`
	Summary   map[string]any `json:"summary"`
	Groups    []struct {
		Size   int64  `json:"size"`
		Digest string `json:"blake2b256"`
		Copies []struct {
			Path      string   `json:"path"`
			MTimeNS   *int64   `json:"mtime_ns"`
			Keep      bool     `json:"keep"`
			Hardlinks []string `json:"hardlinks"`
		} `json:"copies"`
	} `json:"groups"`
}

// Import replaces the current groups with the ones in a report. path may be the file or its folder.
func (a *App) Import(path string) (int, error) {
	path = filepath.Clean(path)
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "duplicates.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("cannot read that report: %w", err)
	}
	var doc reportDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, fmt.Errorf("that is not a duplicates.json report: %w", err)
	}
	var groups []*Group
	for _, jg := range doc.Groups {
		g := &Group{Size: jg.Size, Digest: jg.Digest}
		for _, jc := range jg.Copies {
			if jc.MTimeNS == nil {
				return 0, fmt.Errorf("this report predates delete support (no modified times): scan again")
			}
			g.Copies = append(g.Copies, &Copy{Path: jc.Path, MTime: *jc.MTimeNS, Links: jc.Hardlinks, Keep: jc.Keep})
		}
		if len(g.Copies) >= 2 {
			groups = append(groups, g)
		}
	}

	a.smu.Lock()
	if a.running {
		a.smu.Unlock()
		return 0, errBusy
	}
	a.smu.Unlock()

	a.mu.Lock()
	a.resetGroupsLocked()
	for _, g := range groups {
		a.rev++
		g.Rev = a.rev
		g.ID = len(a.groups) + 1
		pickKeeperIfMissing(g, a.settings.Prefer)
		for _, c := range g.Copies {
			c.Cat, c.Loc = a.cls.Category(c.Path), location(c.Path)
			a.index[normKey(c.Path)] = ref{g, c}
		}
		a.groups = append(a.groups, g)
	}
	a.mu.Unlock()

	nGroups, reclaim := a.liveStats()
	st := a.setStatus(func(s *Status) {
		*s = Status{Phase: "done", Source: "import", Imported: path, Message: "Opened a saved report from " + doc.Generated,
			Groups: nGroups, Reclaimable: reclaim}
		if v, ok := doc.Summary["files_scanned"].(float64); ok {
			s.Files = int64(v)
		}
		if v, ok := doc.Summary["bytes_scanned"].(float64); ok {
			s.Bytes = int64(v)
		}
	})
	a.hub.Broadcast("status", st)
	a.publishGroups()
	go a.Verify() // some files may be gone since the report was written
	return len(groups), nil
}

func pickKeeperIfMissing(g *Group, prefer []string) {
	for _, c := range g.Copies {
		if c.Keep {
			return
		}
	}
	pickKeeper(g, prefer)
}

type ReportInfo struct {
	Path     string `json:"path"`
	Modified string `json:"modified"`
	Size     int64  `json:"size"`
}

// discoverReports finds duplicates.json files written by the earlier script, directly in each base folder or in a
// folder one level below it, newest first. It saves typing a path.
func discoverReports(bases []string) []ReportInfo {
	seen := map[string]bool{}
	type found struct {
		info ReportInfo
		at   time.Time
	}
	var all []found
	add := func(p string) {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && !seen[normKey(p)] {
			seen[normKey(p)] = true
			all = append(all, found{ReportInfo{p, st.ModTime().Format("2006-01-02 15:04"), st.Size()}, st.ModTime()})
		}
	}
	for _, b := range bases {
		if b == "" {
			continue
		}
		add(filepath.Join(b, "duplicates.json"))
		ents, _ := os.ReadDir(b)
		for _, e := range ents {
			if e.IsDir() {
				add(filepath.Join(b, e.Name(), "duplicates.json"))
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	out := []ReportInfo{}
	for _, f := range all {
		out = append(out, f.info)
	}
	return out
}

// reportBases are the places worth looking: next to the program (and a few levels up), the data folder, and the
// usual user folders.
func (a *App) reportBases() []string {
	var bases []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		for i := 0; i < 3; i++ {
			bases = append(bases, d)
			d = filepath.Dir(d)
		}
	}
	bases = append(bases, a.dataDir)
	if home := os.Getenv("USERPROFILE"); home != "" {
		for _, sub := range []string{"Downloads", "Desktop", "Documents"} {
			bases = append(bases, filepath.Join(home, sub))
		}
	}
	return bases
}
