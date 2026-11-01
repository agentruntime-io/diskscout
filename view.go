package main

// Reading the tree for the window: what the map shows for a folder, with the toggles applied.
//
//	HideSys    leave out system-managed items (Recycle Bin, pagefile.sys, System Volume Information, ...)
//	ShowFree   on a drive: a block for the free space
//	ShowUnacc  on a drive: a block for what the disk reports as used but the scan could not see
//	Filter     only files that match, with folders sized by what matches inside them

import (
	"sort"
	"strconv"
	"strings"
)

// Item is one block of the map: a folder, a file, a lump of small things, or a drive-level reconciliation block.
type Item struct {
	Name  string  `json:"name"`
	Path  string  `json:"path,omitempty"`
	Kind  string  `json:"kind"` // dir | file | small | free | unacc
	Size  int64   `json:"size"`
	Files int64   `json:"files,omitempty"`
	Done  bool    `json:"done"`
	Ext   string  `json:"ext,omitempty"`
	MTime int64   `json:"mtime,omitempty"`
	Dup   bool    `json:"dup,omitempty"`
	Sys   bool    `json:"sys,omitempty"`
	Type  string  `json:"type,omitempty"` // files: video, archive, image, doc, audio, code, exe or other
	Dom   string  `json:"dom,omitempty"`  // folders: the class that holds most of the bytes
	Pure  float64 `json:"pure,omitempty"` // ... and its share, 0 to 1
	// Kinds is, for the folder being viewed (not its children), how many bytes of each class are in it, every file counted.
	Kinds    map[string]int64 `json:"kinds,omitempty"`
	Children []Item           `json:"children,omitempty"`
	node     *Node
}

// nodeTypes is the bytes per file class in a folder and everything below it. A folder still being read is summed from
// what is known so far.
func nodeTypes(n *Node) [8]int64 {
	n.mu.Lock()
	done, types, own, rest := n.Done, n.Types, n.OwnTypes, n.RestTypes
	dirs := append([]*Node(nil), n.Dirs...)
	n.mu.Unlock()
	var sum int64
	for _, v := range types {
		sum += v
	}
	if done && sum > 0 {
		return types
	}
	var out [8]int64
	for i := range out {
		out[i] = own[i] + rest[i]
	}
	for _, d := range dirs {
		for i, v := range nodeTypes(d) {
			out[i] += v
		}
	}
	return out
}

func kindsMap(t [8]int64) map[string]int64 {
	m := map[string]int64{}
	for i, v := range t {
		if v > 0 {
			m[typeOrder[i]] = v
		}
	}
	return m
}

type Crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ViewOpts struct {
	Depth, Limit, Budget int
	HideSys              bool
	ShowFree, ShowUnacc  bool
	Filter               *Filter
	Dup                  func(string) bool
}

type meas struct {
	size, cnt int64
	done      bool
}

type viewCtx struct {
	t      *Tree
	o      ViewOpts
	memo   map[*Node]meas
	budget int
}

func isDriveRoot(name string) bool { return len(name) == 3 && name[1] == ':' && name[2] == '\\' }

// View describes the folder at path (empty = all roots) with its biggest children, nested down to o.Depth levels.
func (t *Tree) View(path string, o ViewOpts) (Item, []Crumb, bool) {
	n, exact := t.Root.find(path)
	if n == nil || !exact {
		return Item{}, nil, false
	}
	if o.Budget <= 0 {
		o.Budget = 1500
	}
	c := &viewCtx{t: t, o: o, memo: map[*Node]meas{}, budget: o.Budget}
	m := c.measure(n, path)
	it := Item{Name: n.Name, Path: path, Kind: "dir", Size: m.size, Files: m.cnt, Done: m.done, Kinds: kindsMap(nodeTypes(n))}
	if n == t.Root {
		it.Name = "This PC"
	}
	free, unacc, _ := c.driveExtra(n)
	it.Size += free + unacc
	it.Children = c.items(n, path, o.Depth, o.Limit)
	crumbs := []Crumb{{"This PC", ""}}
	if n != t.Root {
		var chain []*Node
		for p := n; p != nil && p != t.Root; p = p.Parent {
			chain = append([]*Node{p}, chain...)
		}
		for _, p := range chain {
			crumbs = append(crumbs, Crumb{p.Name, p.path()})
		}
	}
	return it, crumbs, true
}

// driveExtra returns the free space and the unaccounted space of a drive-root node, for the toggles that are on.
// Unaccounted is what the disk calls used minus what the scan found: system areas it cannot read, shadow copies,
// folders the user told it to skip. It is only meaningful for a drive whose scan is complete.
func (c *viewCtx) driveExtra(n *Node) (free, unacc int64, ok bool) {
	if n.Parent != c.t.Root || n.Parent == nil || !isDriveRoot(n.Name) || c.o.Filter != nil {
		return 0, 0, false
	}
	st := n.stats()
	if !st.Done {
		return 0, 0, false
	}
	f, total, err := diskUsage(n.Name)
	if err != nil {
		return 0, 0, false
	}
	if c.o.ShowFree {
		free = f
	}
	if c.o.ShowUnacc {
		if u := total - f - st.Total; u > 1<<20 {
			unacc = u
		}
	}
	return free, unacc, true
}

// measure is the size and file count of a folder as the current toggles and filter see it.
func (c *viewCtx) measure(n *Node, base string) meas {
	if !c.o.HideSys && c.o.Filter == nil {
		t, k, d := n.sums()
		return meas{t, k, d}
	}
	if m, ok := c.memo[n]; ok {
		return m
	}
	if c.o.Filter == nil { // only hiding system items: subtract what is system-managed
		st := n.stats()
		m := meas{st.Total - st.Sys, st.N, st.Done}
		c.memo[n] = m
		return m
	}
	n.mu.Lock()
	dirs := append([]*Node(nil), n.Dirs...)
	files := append([]FileEnt(nil), n.Files...)
	done := n.Done
	n.mu.Unlock()
	var m meas
	m.done = done
	for _, f := range files {
		if c.o.HideSys && f.Sys {
			continue
		}
		p := joinPath(base, f.Name)
		if c.o.Filter.Match(p, f.Name, f.Size, f.MTime, c.o.Dup != nil && c.o.Dup(p)) {
			m.size += f.Size
			m.cnt++
		}
	}
	for _, d := range dirs {
		d.mu.Lock()
		sys, dn := d.Sys, d.Name
		d.mu.Unlock()
		if c.o.HideSys && sys {
			continue
		}
		k := c.measure(d, joinPath(base, dn))
		m.size += k.size
		m.cnt += k.cnt
		if !k.done {
			m.done = false
		}
	}
	c.memo[n] = m
	return m
}

func (c *viewCtx) items(n *Node, base string, depth, limit int) []Item {
	n.mu.Lock()
	dirs := append([]*Node(nil), n.Dirs...)
	files := append([]FileEnt(nil), n.Files...)
	own, ownN, ownSys, rest, restN, restSys := n.Own, n.OwnN, n.OwnSys, n.Rest, n.RestN, n.RestSys
	n.mu.Unlock()

	out := make([]Item, 0, len(dirs)+len(files)+4)
	for _, d := range dirs {
		d.mu.Lock()
		sys, dn, types := d.Sys, d.Name, d.Types
		if !d.Done {
			types = d.OwnTypes // still being read: what is known so far
		}
		d.mu.Unlock()
		if c.o.HideSys && sys {
			continue
		}
		m := c.measure(d, joinPath(base, dn))
		if m.size <= 0 {
			continue
		}
		dom, pure := dominant(types)
		it := Item{Name: dn, Path: joinPath(base, dn), Kind: "dir", Size: m.size, Files: m.cnt, Done: m.done, Sys: sys, Dom: dom, Pure: float64(int(pure*100)) / 100, node: d}
		free, unacc, _ := c.driveExtra(d) // a drive shown inside "This PC" is as big as the drive, not just what was found
		it.Size += free + unacc
		out = append(out, it)
	}
	var listed, listedSys int64
	for _, f := range files {
		listed += f.Size
		if f.Sys {
			listedSys += f.Size
		}
		if c.o.HideSys && f.Sys {
			continue
		}
		p := joinPath(base, f.Name)
		dup := c.o.Dup != nil && c.o.Dup(p)
		if c.o.Filter != nil && !c.o.Filter.Match(p, f.Name, f.Size, f.MTime, dup) {
			continue
		}
		ext := ""
		if i := strings.LastIndexByte(f.Name, '.'); i >= 0 {
			ext = strings.ToLower(f.Name[i+1:])
		}
		out = append(out, Item{Name: f.Name, Path: p, Kind: "file", Size: f.Size, Files: 1, Done: true, MTime: f.MTime, Ext: ext, Type: fileTypeOf(f.Name), Dup: dup, Sys: f.Sys})
	}
	if c.o.Filter == nil { // the lumps of small things only make sense when nothing is being filtered out
		small, smallFolders := own-listed, rest
		if c.o.HideSys {
			small -= ownSys - listedSys
			smallFolders -= restSys
		}
		if small > 0 {
			out = append(out, Item{Name: "files under " + fmtBytes(c.t.MinFile), Kind: "small", Size: small, Files: ownN - int64(len(files)), Done: true})
		}
		if smallFolders > 0 {
			out = append(out, Item{Name: "small folders", Kind: "small", Size: smallFolders, Files: restN, Done: true})
		}
		if free, unacc, ok := c.driveExtra(n); ok {
			if unacc > 0 {
				out = append(out, Item{Name: "Unaccounted", Kind: "unacc", Size: unacc, Done: true})
			}
			if free > 0 {
				out = append(out, Item{Name: "Free space", Kind: "free", Size: free, Done: true})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Size > out[b].Size })
	if limit > 0 && len(out) > limit {
		var sum, cnt int64
		for _, it := range out[limit:] {
			sum += it.Size
			cnt += it.Files
		}
		more := len(out) - limit
		out = append(out[:limit:limit], Item{Name: "+" + strconv.Itoa(more) + " more", Kind: "small", Size: sum, Files: cnt, Done: true})
	}
	c.budget -= len(out)
	if depth > 1 {
		for i := range out { // biggest first, so the folders worth looking inside get nested before the budget runs out
			if c.budget <= 0 {
				break
			}
			if out[i].Kind == "dir" && out[i].node != nil {
				out[i].Children = c.items(out[i].node, out[i].Path, depth-1, limit/2+2)
			}
		}
	}
	return out
}
