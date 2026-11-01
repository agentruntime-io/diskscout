package main

// The folder view has to say more than "180 files here are identical to files there": it has to say "...out of 200 here
// and 250 there", or the claim cannot be checked. The duplicate search only looks at files above a size, so after a scan
// the app remembers, for each folder, the files that took part, and answers from that. (A saved report carries no such
// list, so it has no totals.)

import (
	"os"
	"path/filepath"
	"strings"
)

type dirFile struct {
	Name string
	Size int64
}

type dirEnt struct {
	path  string
	files []dirFile
}

// buildDirIndex groups the duplicate-search candidates by the folder that holds them, keyed like the rest of the app.
func buildDirIndex(cands []Candidate) map[string]*dirEnt {
	m := map[string]*dirEnt{}
	for _, c := range cands {
		dir := filepath.Dir(c.Path)
		k := normKey(dir)
		e := m[k]
		if e == nil {
			e = &dirEnt{path: dir}
			m[k] = e
		}
		e.files = append(e.files, dirFile{filepath.Base(c.Path), c.Size})
	}
	return m
}

type DirFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// DirInfo is what the duplicate search saw in one folder (not its subfolders) and that is still there.
type DirInfo struct {
	Known bool      `json:"known"` // false when this session has no scan to ask (a saved report was opened instead)
	Files int       `json:"files"`
	Bytes int64     `json:"bytes"`
	List  []DirFile `json:"list,omitempty"`
}

// inScan says whether the last scan saw this file (a duplicate-search candidate), so the folder tools only act on
// what the app listed.
func (a *App) inScan(p string) bool {
	a.mu.RLock()
	idx := a.dirs
	a.mu.RUnlock()
	e := idx[normKey(filepath.Dir(p))]
	if e == nil {
		return false
	}
	base := filepath.Base(p)
	for _, f := range e.files {
		if strings.EqualFold(f.Name, base) {
			return true
		}
	}
	return false
}

// inScannedRoots says whether a folder lies inside one of the folders that were scanned.
func (a *App) inScannedRoots(p string) bool {
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	if t == nil {
		return false
	}
	for _, r := range t.Roots {
		if hasPathPrefix(p, r) {
			return true
		}
	}
	return false
}

const maxDirList = 3000

// DirFiles reports, for each folder, how many files the duplicate search looked at there and how big they are,
// counting only the ones that still exist. With list set it names them as well.
func (a *App) DirFiles(dirs []string, list bool) map[string]DirInfo {
	a.mu.RLock()
	idx := a.dirs
	a.mu.RUnlock()
	out := make(map[string]DirInfo, len(dirs))
	for _, d := range dirs {
		e := idx[normKey(d)]
		if e == nil {
			out[d] = DirInfo{Known: idx != nil}
			continue
		}
		info := DirInfo{Known: true}
		for _, f := range e.files {
			st, err := os.Lstat(filepath.Join(e.path, f.Name))
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			info.Files++
			info.Bytes += st.Size()
			if list && len(info.List) < maxDirList {
				info.List = append(info.List, DirFile{f.Name, st.Size()})
			}
		}
		out[d] = info
	}
	return out
}
