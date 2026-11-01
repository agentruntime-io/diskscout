package main

// The space map: a tree of folders with sizes, built by reading directories in parallel in the background.
//
// Memory stays bounded however big the disk is: when a folder is complete, subfolders under MinNode are folded into
// the parent's "smaller folders" total, and only the biggest MaxFiles files per folder are kept by name. The totals
// stay exact; only the detail of the small stuff is dropped.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type FileEnt struct {
	Name  string
	Size  int64
	MTime int64
	Sys   bool // Windows marks it as a system file (pagefile.sys, hiberfil.sys, ...)
}

// Node is one folder. The virtual root (Name "") holds the scan roots as children.
type Node struct {
	mu        sync.Mutex
	Name      string // scan roots hold their full path ("C:\Users"); everything below holds just its own name
	Parent    *Node
	Dirs      []*Node
	Files     []FileEnt // the biggest files directly in this folder
	Own       int64     // bytes of all files directly in this folder, listed or not
	OwnN      int64
	OwnSys    int64 // of which bytes in system files
	Rest      int64 // bytes in subfolders that were folded away
	RestN     int64
	RestSys   int64 // of which bytes in system-managed folders
	Total     int64 // valid once Done
	TotalN    int64
	SysTotal  int64    // bytes in the subtree that are system-managed: a system folder counts as a whole
	OwnTypes  [8]int64 // bytes per file class directly in this folder (all files, listed or not)
	RestTypes [8]int64 // ... in subfolders that were folded away
	Types     [8]int64 // ... in the whole subtree, once Done. After a deletion it is left as it was until the next scan.
	Sys       bool     // Windows marks this folder as a system folder ($Recycle.Bin, System Volume Information, ...)
	Done      bool
	pending   int32
}

const attrSystem = 0x4

type nodeStats struct {
	Total, N, Sys int64
	Done          bool
}

// stats gives the size, file count and system-managed bytes of the subtree. For a folder still being read it is the
// partial figure so far.
func (n *Node) stats() nodeStats {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.Done {
		return nodeStats{n.Total, n.TotalN, n.SysTotal, true}
	}
	total, count, sys := n.Own+n.Rest, n.OwnN+n.RestN, n.OwnSys+n.RestSys
	for _, c := range n.Dirs {
		k := c.stats()
		total += k.Total
		count += k.N
		sys += k.Sys
	}
	if n.Sys {
		sys = total
	}
	return nodeStats{total, count, sys, false}
}

func (n *Node) sums() (total, count int64, done bool) {
	s := n.stats()
	return s.Total, s.N, s.Done
}

func (n *Node) path() string {
	var names []string
	for p := n; p != nil && p.Name != ""; p = p.Parent {
		names = append(names, p.Name)
	}
	var path string
	for i := len(names) - 1; i >= 0; i-- {
		path = joinPath(path, names[i])
	}
	return path
}

// find returns the node for a path, or the deepest node that exists on the way (exact=false) if the rest was folded away.
func (r *Node) find(path string) (n *Node, exact bool) {
	if path == "" {
		return r, true
	}
	path = filepath.Clean(path)
	r.mu.Lock()
	roots := append([]*Node(nil), r.Dirs...)
	r.mu.Unlock()
	var cur *Node
	for _, c := range roots {
		if hasPathPrefix(path, c.Name) {
			cur = c
			break
		}
	}
	if cur == nil {
		return nil, false
	}
	rest := strings.TrimLeft(path[len(strings.TrimRight(cur.Name, `\`)):], `\`)
	if rest == "" {
		return cur, true
	}
	for _, seg := range strings.Split(rest, `\`) {
		var next *Node
		cur.mu.Lock()
		for _, c := range cur.Dirs {
			if strings.EqualFold(c.Name, seg) {
				next = c
				break
			}
		}
		cur.mu.Unlock()
		if next == nil {
			return cur, false
		}
		cur = next
	}
	return cur, true
}

type ScanOpts struct {
	Roots       []string
	Excludes    []string // never entered, never counted
	DupExcludes []string // counted in the map, but not offered to the duplicate finder
	MinNode     int64    // folders smaller than this are folded into their parent
	MinFile     int64    // files smaller than this are not listed by name
	MaxFiles    int      // listed files per folder
	MinDup      int64    // files at least this big become duplicate candidates; 0 turns the duplicate search off
	Workers     int
}

type Candidate struct {
	Path  string
	Size  int64
	MTime int64
}

type ScanError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type Tree struct {
	Root     *Node
	Roots    []string
	MinFile  int64
	MinNode  int64
	Started  time.Time
	Finished time.Time
	Complete bool // false if cancelled
}

type Scanner struct {
	opts    ScanOpts
	tree    *Tree
	excl    excludeRules
	dupExcl excludeRules

	Files, Dirs, Bytes, Errs atomic.Int64
	current                  atomic.Value // string: a folder being read, for the status line

	errMu   sync.Mutex
	errList []ScanError
	candMu  sync.Mutex
	cands   []Candidate

	qmu    sync.Mutex
	qcond  *sync.Cond
	stack  []job
	active int
}

type job struct {
	n     *Node
	path  string
	noDup bool // inside a folder the duplicate search skips: nothing below it is offered either
}

func newScanner(opts ScanOpts) *Scanner {
	if opts.Workers <= 0 {
		opts.Workers = 16
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 300
	}
	s := &Scanner{opts: opts, excl: parseExcludes(opts.Excludes), dupExcl: parseExcludes(opts.DupExcludes)}
	s.qcond = sync.NewCond(&s.qmu)
	s.tree = &Tree{Root: &Node{}, MinFile: opts.MinFile, MinNode: opts.MinNode, Started: time.Now()}
	s.current.Store("")
	return s
}

func (s *Scanner) Tree() *Tree             { return s.tree }
func (s *Scanner) Candidates() []Candidate { s.candMu.Lock(); defer s.candMu.Unlock(); return s.cands }
func (s *Scanner) Current() string         { v, _ := s.current.Load().(string); return v }
func (s *Scanner) ErrorList() []ScanError {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return append([]ScanError(nil), s.errList...)
}

func (s *Scanner) recordErr(path string, err error) {
	s.Errs.Add(1)
	s.errMu.Lock()
	if len(s.errList) < 500 {
		s.errList = append(s.errList, ScanError{path, err.Error()})
	}
	s.errMu.Unlock()
}

// Run reads every root and blocks until done or cancelled. The tree can be read (partially) while it runs.
func (s *Scanner) Run(ctx context.Context) {
	roots := normalizeRoots(s.opts.Roots)
	s.tree.Roots = roots
	var first []job
	for _, r := range roots {
		n := &Node{Name: r, Parent: s.tree.Root, pending: 1}
		s.tree.Root.Dirs = append(s.tree.Root.Dirs, n)
		first = append(first, job{n, r, s.dupExcl.match(filepath.Base(r), r)})
	}
	s.push(first)

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.qmu.Lock()
			s.qmu.Unlock()
			s.qcond.Broadcast()
		case <-stop:
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < s.opts.Workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.worker(ctx) }()
	}
	wg.Wait()
	close(stop)

	root := s.tree.Root
	root.mu.Lock()
	root.Total, root.TotalN = 0, 0
	for _, c := range root.Dirs {
		t, k, _ := c.sums()
		root.Total += t
		root.TotalN += k
	}
	root.Done = ctx.Err() == nil
	root.mu.Unlock()
	s.tree.Complete = ctx.Err() == nil
	s.tree.Finished = time.Now()
}

func (s *Scanner) push(jobs []job) {
	if len(jobs) == 0 {
		return
	}
	s.qmu.Lock()
	s.stack = append(s.stack, jobs...)
	s.qmu.Unlock()
	s.qcond.Broadcast()
}

func (s *Scanner) worker(ctx context.Context) {
	for {
		s.qmu.Lock()
		for len(s.stack) == 0 && s.active > 0 && ctx.Err() == nil {
			s.qcond.Wait()
		}
		if ctx.Err() != nil || len(s.stack) == 0 {
			s.qmu.Unlock()
			s.qcond.Broadcast()
			return
		}
		j := s.stack[len(s.stack)-1] // newest first: finishes subtrees early, so memory is released early
		s.stack = s.stack[:len(s.stack)-1]
		s.active++
		s.qmu.Unlock()

		s.scanDir(j)

		s.qmu.Lock()
		s.active--
		idle := s.active == 0 && len(s.stack) == 0
		s.qmu.Unlock()
		if idle {
			s.qcond.Broadcast()
		}
	}
}

func (s *Scanner) scanDir(j job) {
	s.current.Store(j.path)
	f, err := os.Open(j.path)
	if err != nil {
		s.recordErr(j.path, err)
		s.release(j.n)
		return
	}
	var (
		own, ownN int64
		ownSys    int64
		ownTypes  [8]int64
		files     []FileEnt
		dirs      []*Node
		dirJobs   []job
		cands     []Candidate
	)
	for {
		ents, err := f.ReadDir(512)
		for _, e := range ents {
			name := e.Name()
			full := joinPath(j.path, name)
			if s.excl.match(name, full) {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				s.recordErr(full, ierr)
				continue
			}
			attrs := fileAttrs(info)
			if attrs&skipAttrs != 0 {
				continue
			}
			sys := attrs&attrSystem != 0
			if info.IsDir() {
				child := &Node{Name: name, Parent: j.n, pending: 1, Sys: sys}
				dirs = append(dirs, child)
				dirJobs = append(dirJobs, job{child, full, j.noDup || s.dupExcl.match(name, full)})
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			size := info.Size()
			own += size
			ownN++
			if sys {
				ownSys += size
			}
			ownTypes[typeIdxOf(name)] += size
			if size >= s.opts.MinFile {
				files = append(files, FileEnt{name, size, info.ModTime().UnixNano(), sys})
			}
			if s.opts.MinDup > 0 && size >= s.opts.MinDup && !j.noDup && !s.dupExcl.match(name, full) {
				cands = append(cands, Candidate{full, size, info.ModTime().UnixNano()})
			}
		}
		if err != nil {
			if err != io.EOF {
				s.recordErr(j.path, err)
			}
			break
		}
	}
	f.Close()

	s.Files.Add(ownN)
	s.Bytes.Add(own)
	s.Dirs.Add(1)
	if len(cands) > 0 {
		s.candMu.Lock()
		s.cands = append(s.cands, cands...)
		s.candMu.Unlock()
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Size > files[b].Size })
	if len(files) > s.opts.MaxFiles {
		files = files[:s.opts.MaxFiles:s.opts.MaxFiles]
	}
	j.n.mu.Lock()
	j.n.Dirs, j.n.Files, j.n.Own, j.n.OwnN, j.n.OwnSys, j.n.OwnTypes = dirs, files, own, ownN, ownSys, ownTypes
	j.n.mu.Unlock()

	atomic.AddInt32(&j.n.pending, int32(len(dirs)))
	s.push(dirJobs)
	s.release(j.n)
}

// release marks one unit of work on n as finished; when the last one is, n is complete and its parent loses one.
func (s *Scanner) release(n *Node) {
	for n != nil && n != s.tree.Root {
		if atomic.AddInt32(&n.pending, -1) != 0 {
			return
		}
		s.finalize(n)
		n = n.Parent
	}
}

func (s *Scanner) finalize(n *Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var keptTotal, keptN, keptSys, pruned, prunedN, prunedSys int64
	var keptTypes [8]int64
	kept := make([]*Node, 0, len(n.Dirs))
	for _, c := range n.Dirs {
		c.mu.Lock()
		ct, cn, cs, cty := c.Total, c.TotalN, c.SysTotal, c.Types
		c.mu.Unlock()
		if ct < s.opts.MinNode {
			pruned += ct
			prunedN += cn
			prunedSys += cs
			for i, v := range cty {
				n.RestTypes[i] += v
			}
		} else {
			kept = append(kept, c)
			keptTotal += ct
			keptN += cn
			keptSys += cs
			for i, v := range cty {
				keptTypes[i] += v
			}
		}
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].Total > kept[b].Total })
	n.Dirs = kept
	n.Rest += pruned
	n.RestN += prunedN
	n.RestSys += prunedSys
	n.Total = n.Own + n.Rest + keptTotal
	n.TotalN = n.OwnN + n.RestN + keptN
	n.SysTotal = n.OwnSys + n.RestSys + keptSys
	for i := range n.Types {
		n.Types[i] = n.OwnTypes[i] + n.RestTypes[i] + keptTypes[i]
	}
	if n.Sys {
		n.SysTotal = n.Total // a system folder is system-managed as a whole
	}
	n.Done = true
}

// ---------------------------------------------------------------- keeping it in step with the disk

// forgetFile removes a deleted file's bytes from the map. A file in a folded-away folder comes off the nearest folder
// that is still on the map, so the totals stay right.
func (t *Tree) forgetFile(path string, size int64) {
	n, exact := t.Root.find(filepath.Dir(path))
	if n == nil || n == t.Root {
		return
	}
	var dSys int64
	n.mu.Lock()
	if exact {
		name := filepath.Base(path)
		for i, f := range n.Files {
			if strings.EqualFold(f.Name, name) {
				if f.Sys {
					dSys = -size
					n.OwnSys -= size
				}
				n.Files = append(n.Files[:i], n.Files[i+1:]...)
				break
			}
		}
		n.Own -= size
		n.OwnN--
	} else {
		n.Rest -= size
		n.RestN--
	}
	n.mu.Unlock()
	t.adjustUp(n, -size, -1, dSys)
}

// forgetDir removes a whole folder (which must be on the map) after it was deleted.
func (t *Tree) forgetDir(path string) (size, count int64, ok bool) {
	n, exact := t.Root.find(path)
	if n == nil || !exact || n == t.Root || n.Parent == nil {
		return 0, 0, false
	}
	st := n.stats()
	p := n.Parent
	p.mu.Lock()
	for i, c := range p.Dirs {
		if c == n {
			p.Dirs = append(p.Dirs[:i], p.Dirs[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	t.adjustUp(p, -st.Total, -st.N, -st.Sys)
	return st.Total, st.N, true
}

// adjustUp changes the totals of n and every folder above it. dSys is the part of the change that was already
// counted as system-managed; once the walk passes a system folder, the whole change counts as system-managed.
func (t *Tree) adjustUp(n *Node, dSize, dCount, dSys int64) {
	for p := n; p != nil; p = p.Parent {
		p.mu.Lock()
		if p.Sys {
			dSys = dSize
		}
		if p.Done {
			p.Total += dSize
			p.TotalN += dCount
			p.SysTotal += dSys
		}
		p.mu.Unlock()
	}
}

// bumpRecycle accounts for a file that was moved to the Recycle Bin: it left its folder but not the disk, so the
// bin's block grows by the same amount and "free space" does not pretend anything was reclaimed.
func (t *Tree) bumpRecycle(path string, size int64, count int64) {
	drive := filepath.VolumeName(path)
	if drive == "" {
		return
	}
	bin, exact := t.Root.find(drive + `\$Recycle.Bin`)
	if bin == nil || !exact {
		return
	}
	bin.mu.Lock()
	bin.Rest += size
	bin.RestN += count
	bin.mu.Unlock()
	t.adjustUp(bin, size, count, 0)
}

// rescan reads one folder again and swaps the result in, so a change made outside the app shows up without a full scan.
func (t *Tree) rescan(ctx context.Context, path string, opts ScanOpts) (int64, bool) {
	old, exact := t.Root.find(path)
	if old == nil || !exact || old == t.Root || old.Parent == nil {
		return 0, false
	}
	opts.Roots = []string{path}
	opts.MinDup = 0
	sc := newScanner(opts)
	sc.Run(ctx)
	if ctx.Err() != nil || len(sc.tree.Root.Dirs) == 0 {
		return 0, false
	}
	fresh := sc.tree.Root.Dirs[0]
	oldSt := old.stats()
	fresh.mu.Lock()
	fresh.Name = old.Name // a scan root carries its full path; here it is just the folder's own name
	fresh.Parent = old.Parent
	fresh.Sys = old.Sys
	if fresh.Sys {
		fresh.SysTotal = fresh.Total
	}
	freshTotal, freshN, freshSys := fresh.Total, fresh.TotalN, fresh.SysTotal
	fresh.mu.Unlock()
	p := old.Parent
	p.mu.Lock()
	for i, c := range p.Dirs {
		if c == old {
			p.Dirs[i] = fresh
			break
		}
	}
	p.mu.Unlock()
	t.adjustUp(p, freshTotal-oldSt.Total, freshN-oldSt.N, freshSys-oldSt.Sys)
	return freshTotal - oldSt.Total, true
}
