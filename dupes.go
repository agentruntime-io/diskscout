package main

// Finding duplicates by content. Most files are never fully read; candidates are narrowed in stages:
//   1. size       a file with a unique size can't have a duplicate (free: it came with the directory listing)
//   2. hardlinks  names that point at the same on-disk file count as ONE copy: there is nothing to reclaim
//   3. head+tail  a hash of the first and last 64 KB of each file still tied
//   4. full hash  SHA-256 of the whole file, only for files still tied after step 3
// Biggest sizes go first, so the largest wins show up while the rest is still running. Hashes are cached by
// (path, size, modified time), so a second run only reads files that are new or changed.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const partSize = 64 << 10

type Copy struct {
	Path  string
	MTime int64
	Links []string // other names of the same physical file
	Keep  bool
	Gone  bool
	Cat   int
	Loc   string
}

type Group struct {
	ID     int
	Size   int64
	Digest string
	Copies []*Copy
	Rev    int64
}

func (g *Group) live() []*Copy {
	var out []*Copy
	for _, c := range g.Copies {
		if !c.Gone {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------- cache

type hashCache struct {
	mu   sync.Mutex
	m    map[string]string
	f    *os.File
	w    *bufio.Writer
	Hits atomic.Int64
}

func openHashCache(path string) *hashCache {
	c := &hashCache{m: map[string]string{}}
	if path == "" {
		return c
	}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			p := strings.SplitN(sc.Text(), "\t", 5)
			if len(p) == 5 && len(p[0]) == 1 { // a half-written last line is simply ignored
				c.m[p[0]+"|"+p[1]+"|"+p[2]+"|"+strings.ToLower(p[4])] = p[3]
			}
		}
		f.Close()
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		c.f, c.w = f, bufio.NewWriter(f)
	}
	return c
}

func cacheKey(kind byte, path string, size, mtime int64) string {
	return string(kind) + "|" + strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(mtime, 10) + "|" + strings.ToLower(path)
}

func (c *hashCache) get(kind byte, path string, size, mtime int64) (string, bool) {
	c.mu.Lock()
	d, ok := c.m[cacheKey(kind, path, size, mtime)]
	c.mu.Unlock()
	if ok {
		c.Hits.Add(1)
	}
	return d, ok
}

func (c *hashCache) put(kind byte, path string, size, mtime int64, digest string) {
	c.mu.Lock()
	c.m[cacheKey(kind, path, size, mtime)] = digest
	if c.w != nil {
		fmt.Fprintf(c.w, "%c\t%d\t%d\t%s\t%s\n", kind, size, mtime, digest, path)
	}
	c.mu.Unlock()
}

func (c *hashCache) flush() {
	c.mu.Lock()
	if c.w != nil {
		c.w.Flush()
	}
	c.mu.Unlock()
}

func (c *hashCache) close() {
	c.flush()
	if c.f != nil {
		c.f.Close()
	}
}

// ---------------------------------------------------------------- hashing

func headTailDigest(path string, size int64, buf []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.ReadFull(f, buf[:partSize]); err != nil {
		return "", err
	}
	h.Write(buf[:partSize])
	if _, err := f.Seek(size-partSize, io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.ReadFull(f, buf[:partSize]); err != nil {
		return "", err
	}
	h.Write(buf[:partSize])
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fullDigest(ctx context.Context, path string, buf []byte, onBytes func(int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			onBytes(int64(n))
		}
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// ---------------------------------------------------------------- the pipeline

type DupProgress struct {
	SizeGroups, SizeGroupsDone, BytesHashed, GroupsFound atomic.Int64
	current                                              atomic.Value
}

func (p *DupProgress) Current() string { v, _ := p.current.Load().(string); return v }

type dupFile struct {
	path  string
	mtime int64
	links []string
}

// findDuplicates calls emit for each group of identical files it confirms. It returns when done or cancelled.
func findDuplicates(ctx context.Context, cands []Candidate, cache *hashCache, workers int, prefer []string,
	p *DupProgress, onErr func(string, error), emit func(*Group)) {
	if workers <= 0 {
		workers = 4
	}
	bySize := map[int64][]Candidate{}
	for _, c := range cands {
		bySize[c.Size] = append(bySize[c.Size], c)
	}
	var sizes []int64
	for sz, cs := range bySize {
		if len(cs) >= 2 {
			sizes = append(sizes, sz)
		}
	}
	sort.Slice(sizes, func(a, b int) bool { return sizes[a] > sizes[b] })
	p.SizeGroups.Store(int64(len(sizes)))

	jobs := make(chan int64)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1<<20)
			for sz := range jobs {
				if ctx.Err() == nil {
					processSize(ctx, sz, bySize[sz], cache, buf, prefer, p, onErr, emit)
				}
				p.SizeGroupsDone.Add(1)
			}
		}()
	}
	for _, sz := range sizes {
		if ctx.Err() != nil {
			break
		}
		jobs <- sz
	}
	close(jobs)
	wg.Wait()
	cache.flush()
}

func processSize(ctx context.Context, size int64, cs []Candidate, cache *hashCache, buf []byte, prefer []string,
	p *DupProgress, onErr func(string, error), emit func(*Group)) {
	// 2. names of the same physical file are one copy
	byID := map[fileKey]*dupFile{}
	var uniq []*dupFile
	for _, c := range cs {
		key, _, err := fileIdentity(c.Path)
		if err != nil {
			onErr(c.Path, err)
			continue
		}
		if f, ok := byID[key]; ok {
			f.links = append(f.links, c.Path)
			continue
		}
		f := &dupFile{path: c.Path, mtime: c.MTime}
		byID[key] = f
		uniq = append(uniq, f)
	}
	if len(uniq) < 2 {
		return
	}

	// 3. head + tail (only meaningful when the two ends can't overlap)
	buckets := [][]*dupFile{uniq}
	if size > 2*partSize {
		buckets = splitBy(ctx, uniq, onErr, func(f *dupFile) (string, error) {
			if d, ok := cache.get('h', f.path, size, f.mtime); ok {
				return d, nil
			}
			p.current.Store(f.path)
			d, err := headTailDigest(f.path, size, buf)
			if err == nil {
				cache.put('h', f.path, size, f.mtime, d)
			}
			return d, err
		})
	}

	// 4. full content
	for _, b := range buckets {
		if ctx.Err() != nil {
			return
		}
		digests := map[string][]*dupFile{}
		for _, f := range b {
			if ctx.Err() != nil {
				return
			}
			d, ok := cache.get('f', f.path, size, f.mtime)
			if !ok {
				p.current.Store(f.path)
				var err error
				if d, err = fullDigest(ctx, f.path, buf, func(n int64) { p.BytesHashed.Add(n) }); err != nil {
					if ctx.Err() == nil {
						onErr(f.path, err)
					}
					continue
				}
				cache.put('f', f.path, size, f.mtime, d)
			}
			digests[d] = append(digests[d], f)
		}
		for d, fs := range digests {
			if len(fs) < 2 {
				continue
			}
			g := &Group{Size: size, Digest: d}
			for _, f := range fs {
				g.Copies = append(g.Copies, &Copy{Path: f.path, MTime: f.mtime, Links: f.links})
			}
			pickKeeper(g, prefer)
			p.GroupsFound.Add(1)
			emit(g)
		}
	}
}

// splitBy buckets files by a digest, keeping only buckets that still have two or more files.
func splitBy(ctx context.Context, fs []*dupFile, onErr func(string, error), digest func(*dupFile) (string, error)) [][]*dupFile {
	m := map[string][]*dupFile{}
	for _, f := range fs {
		if ctx.Err() != nil {
			return nil
		}
		d, err := digest(f)
		if err != nil {
			onErr(f.path, err)
			continue
		}
		m[d] = append(m[d], f)
	}
	var out [][]*dupFile
	for _, b := range m {
		if len(b) >= 2 {
			out = append(out, b)
		}
	}
	return out
}

// pickKeeper marks the suggested copy to keep: inside a preferred folder, else the oldest, else the shortest path.
// It is only a suggestion; the user decides.
func pickKeeper(g *Group, prefer []string) {
	live := g.live()
	if len(live) == 0 {
		return
	}
	rank := func(c *Copy) (int, int64, int, string) {
		pref := 1
		for _, p := range prefer {
			if p != "" && hasPathPrefix(c.Path, p) {
				pref = 0
				break
			}
		}
		return pref, c.MTime, len(c.Path), strings.ToLower(c.Path)
	}
	best := live[0]
	for _, c := range live[1:] {
		a1, a2, a3, a4 := rank(c)
		b1, b2, b3, b4 := rank(best)
		if a1 != b1 && a1 < b1 || a1 == b1 && (a2 < b2 || a2 == b2 && (a3 < b3 || a3 == b3 && a4 < b4)) {
			best = c
		}
	}
	for _, c := range g.Copies {
		c.Keep = c == best
	}
	sort.Slice(g.Copies, func(i, j int) bool { // the keeper first, then by path, so the order is stable
		a, b := g.Copies[i], g.Copies[j]
		if a.Keep != b.Keep {
			return a.Keep
		}
		return strings.ToLower(a.Path) < strings.ToLower(b.Path)
	})
}
