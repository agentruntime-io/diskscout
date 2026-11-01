package main

// Zip files and the folders they were unpacked into.
//
// A zip lists every file inside it. So "is everything in this zip in that folder?" has an exact answer: for each file
// in the zip, is there a file at the matching path in the folder, and does it hold exactly the same bytes? And the other
// way round: is there anything in the folder that is not in the zip? Both are answered by reading, never by guessing from
// names. What cannot be answered says so: a zip in a format Go cannot open, an entry that will not decompress, a folder
// that shares its home with other files so "what else belongs here" is unknowable, a RAR or 7z we cannot read at all.

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Archives we can only point at (a folder with the same name sits beside them): Go has no reader for them.
var otherArchives = map[string]string{".rar": "RAR", ".7z": "7z", ".tar": "tar", ".tgz": "tar.gz"}

type ArchiveInfo struct {
	Zip         string `json:"zip"`
	Size        int64  `json:"size"`
	Kind        string `json:"kind"` // "zip", or what it is when we cannot open it
	Readable    bool   `json:"readable"`
	Entries     int    `json:"entries"` // files inside
	Bytes       int64  `json:"bytes"`   // their size once unpacked
	Base        string `json:"base,omitempty"`
	Strip       bool   `json:"strip,omitempty"` // the zip's one top folder was left off when it was unpacked
	Root        string `json:"root,omitempty"`  // the folder that should hold only what came from the zip, when that is clear
	Same        int    `json:"same"`            // files inside that exist under Base with the same name and size
	Extras      int    `json:"extras"`          // files under Root that are not in the zip; -1 when that cannot be told
	FolderFiles int    `json:"folderFiles,omitempty"`
	Note        string `json:"note,omitempty"`
	Err         string `json:"error,omitempty"`
	// Check is what the last byte-for-byte comparison of this zip with this folder found, if it is still current.
	Check *ArchiveVerdict `json:"check,omitempty"`
}

// State says where a zip stands: "safe" (everything in it is in the folder, byte for byte), "different" (it holds
// something the folder lacks or has changed), "unchecked", or "cannot" (a kind we cannot open, or a zip that will not open).
func (i ArchiveInfo) State() string {
	switch {
	case !i.Readable || i.Base == "":
		return "cannot"
	case i.Check == nil:
		return "unchecked"
	case i.Check.ZipInFolder:
		return "safe"
	}
	return "different"
}

type zipFile struct {
	name string // forward slashes, no leading slash
	size int64
	f    *zip.File
}

// zipFiles lists the files in a zip (not its folders). Entries that would land outside the folder are left out and counted.
func zipFiles(zr *zip.ReadCloser) (files []zipFile, skipped int) {
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		name = path.Clean(strings.TrimLeft(name, "/"))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(filepath.FromSlash(name)) || strings.Contains(name, ":") {
			skipped++
			continue
		}
		files = append(files, zipFile{name, int64(f.UncompressedSize64), f})
	}
	return
}

// commonTop returns the one folder that holds every file in the zip, if there is one.
func commonTop(files []zipFile) (string, bool) {
	top := ""
	for i, f := range files {
		j := strings.IndexByte(f.name, '/')
		if j < 0 {
			return "", false
		}
		if i == 0 {
			top = f.name[:j]
		} else if !strings.EqualFold(top, f.name[:j]) {
			return "", false
		}
	}
	return top, top != ""
}

// diskPath is where an entry of the zip sits under base.
func diskPath(base, name string, strip bool, top string) string {
	if strip {
		name = strings.TrimPrefix(name, top+"/")
	}
	return filepath.Join(base, filepath.FromSlash(name))
}

func dirExists(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// sameCount says how many entries have a file of the same name and size under base.
func sameCount(base string, files []zipFile, strip bool, top string) int {
	n := 0
	for _, f := range files {
		if st, err := os.Stat(diskPath(base, f.name, strip, top)); err == nil && st.Mode().IsRegular() && st.Size() == f.size {
			n++
		}
	}
	return n
}

type nameHit struct {
	dir  string
	size int64
}

// nameIndex maps a file name to the folders (and sizes) the duplicate search saw it in: how a zip that was unpacked
// somewhere unexpected, or into a folder with another name, is found.
func (a *App) nameIndex() map[string][]nameHit {
	a.mu.RLock()
	idx := a.dirs
	a.mu.RUnlock()
	m := make(map[string][]nameHit)
	for _, e := range idx {
		for _, f := range e.files {
			k := strings.ToLower(f.Name)
			m[k] = append(m[k], nameHit{e.path, f.Size})
		}
	}
	return m
}

// rootOf is the folder that should hold only what the zip put there, or "" when that cannot be told.
func rootOf(zipPath, base string, strip bool, top string, single bool) string {
	if !strings.EqualFold(filepath.Clean(base), filepath.Clean(filepath.Dir(zipPath))) {
		return base // a folder of its own, apart from where the zip sits
	}
	if single && !strip {
		return filepath.Join(base, top) // unpacked beside the zip, with its one top folder
	}
	return "" // unpacked in among other files
}

// archiveFor looks inside one zip and finds the folder it was most likely unpacked into.
func (a *App) archiveFor(zipPath string, size int64, idx map[string][]nameHit) ArchiveInfo {
	info := ArchiveInfo{Zip: zipPath, Size: size, Kind: "zip", Extras: -1}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		info.Err = err.Error()
		return info
	}
	defer zr.Close()
	files, skipped := zipFiles(zr)
	info.Readable, info.Entries = true, len(files)
	for _, f := range files {
		info.Bytes += f.size
	}
	if len(files) == 0 {
		info.Note = "The zip holds no files."
		return info
	}
	top, single := commonTop(files)
	dir := filepath.Dir(zipPath)
	stem := strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))

	var bases []string
	seen := map[string]bool{}
	add := func(b string) {
		k := normKey(b)
		if b != "" && !seen[k] && dirExists(b) {
			seen[k] = true
			bases = append(bases, b)
		}
	}
	add(filepath.Join(dir, stem)) // "Extract all" makes a folder named after the zip
	add(dir)                      // "Extract here"
	// anywhere else: where do the biggest files in the zip live?
	big := append([]zipFile(nil), files...)
	sort.Slice(big, func(i, j int) bool { return big[i].size > big[j].size })
	for _, e := range big[:min(len(big), 6)] {
		hits := idx[strings.ToLower(path.Base(e.name))]
		for k, h := range hits {
			if k >= 20 {
				break
			}
			if h.size != e.size {
				continue
			}
			ed := path.Dir(e.name)
			for _, rel := range []string{ed, strings.TrimPrefix(strings.TrimPrefix(ed, top), "/")} {
				if rel == "." || rel == "" {
					add(h.dir)
					continue
				}
				suf := strings.ToLower(`\` + filepath.FromSlash(rel))
				if strings.HasSuffix(strings.ToLower(h.dir), suf) {
					add(h.dir[:len(h.dir)-len(suf)])
				}
			}
		}
	}

	best, bestStrip := -1, false
	for _, b := range bases {
		for _, strip := range []bool{false, true} {
			if strip && !single {
				continue
			}
			if n := sameCount(b, files, strip, top); n > 0 && n > best {
				best, bestStrip, info.Base = n, strip, b
			}
		}
	}
	if best <= 0 || best*4 < len(files) { // not unpacked anywhere we can find, or only a sliver of it
		info.Base, info.Same = "", 0
		return info
	}
	info.Same, info.Strip = best, bestStrip
	info.Root = rootOf(zipPath, info.Base, bestStrip, top, single)
	if info.Root != "" {
		info.Extras = countExtras(info.Root, wantSet(info.Base, files, bestStrip, top), zipPath, nil)
	} else {
		info.Note = "It was unpacked into the same folder as the zip, among other files, so what else belongs there cannot be told."
	}
	if skipped > 0 {
		info.Note = strings.TrimSpace(info.Note + fmt.Sprintf(" %d entries with unsafe paths were not compared.", skipped))
	}
	return info
}

func wantSet(base string, files []zipFile, strip bool, top string) map[string]bool {
	m := make(map[string]bool, len(files))
	for _, f := range files {
		m[normKey(diskPath(base, f.name, strip, top))] = true
	}
	return m
}

const maxWalk = 200000

// countExtras counts (and with names, lists) the files under root that the zip does not contain. The zip itself is not one.
func countExtras(root string, want map[string]bool, zipPath string, names *NameList) int {
	n, seen := 0, 0
	zk := normKey(zipPath)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if seen++; seen > maxWalk {
			return fs.SkipAll
		}
		if k := normKey(p); !want[k] && k != zk {
			n++
			if names != nil {
				if rel, err := filepath.Rel(root, p); err == nil {
					p = rel // named from the folder down, which is how the zip names its own files
				}
				names.add(p)
			}
		}
		return nil
	})
	return n
}

// Archives lists the zips the last scan found (1 MB and up) that were unpacked somewhere, and the ones of other kinds
// that have a folder of the same name beside them. known is false when there is no scan in this window to ask.
func (a *App) Archives() (list []ArchiveInfo, known bool) {
	a.mu.RLock()
	idx := a.dirs
	a.mu.RUnlock()
	if idx == nil {
		return nil, false
	}
	names := a.nameIndex()
	for _, e := range idx {
		for _, f := range e.files {
			ext := strings.ToLower(filepath.Ext(f.Name))
			full := filepath.Join(e.path, f.Name)
			switch {
			case ext == ".zip":
				if _, err := os.Stat(full); err != nil {
					continue // moved or deleted since the scan
				}
				if info := a.archiveFor(full, f.Size, names); info.Base != "" || info.Err != "" {
					info.Check = a.recalled(info.Zip, info.Base, info.Strip)
					list = append(list, info)
				}
			case otherArchives[ext] != "":
				side := filepath.Join(e.path, strings.TrimSuffix(f.Name, filepath.Ext(f.Name)))
				if dirExists(side) {
					n := 0
					filepath.WalkDir(side, func(p string, d fs.DirEntry, err error) error {
						if err == nil && !d.IsDir() {
							if n++; n > maxWalk {
								return fs.SkipAll
							}
						}
						return nil
					})
					list = append(list, ArchiveInfo{Zip: full, Size: f.Size, Kind: otherArchives[ext], Base: side, Root: side, Extras: -1, FolderFiles: n,
						Note: "DiskScout can only open .zip archives, so it cannot say whether the folder holds everything in this " + otherArchives[ext] + "."})
				}
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { // zips that could not be opened go last: there is nothing to say about them
		if ei, ej := list[i].Err != "", list[j].Err != ""; ei != ej {
			return ej
		}
		return list[i].Size > list[j].Size
	})
	return list, true
}

// ArchiveFor finds the folder one named zip was unpacked into, for a caller that has the zip's path (Claude).
func (a *App) ArchiveFor(zipPath string) (ArchiveInfo, error) {
	st, err := os.Stat(zipPath)
	if err != nil {
		return ArchiveInfo{}, err
	}
	if !strings.EqualFold(filepath.Ext(zipPath), ".zip") {
		if k := otherArchives[strings.ToLower(filepath.Ext(zipPath))]; k != "" {
			return ArchiveInfo{Zip: zipPath, Size: st.Size(), Kind: k, Extras: -1, Note: "DiskScout can only open .zip archives, so it cannot compare this " + k + " with a folder."}, nil
		}
		return ArchiveInfo{}, errors.New("that is not a zip file")
	}
	info := a.archiveFor(zipPath, st.Size(), a.nameIndex())
	if info.Base != "" {
		info.Check = a.recalled(info.Zip, info.Base, info.Strip)
	}
	return info, nil
}

// ---------------------------------------------------------------- the proof

// NameList is a count and the first few names.
type NameList struct {
	N     int      `json:"n"`
	Names []string `json:"names,omitempty"`
}

const maxNames = 100

func (l *NameList) add(name string) {
	l.N++
	if len(l.Names) < maxNames {
		l.Names = append(l.Names, name)
	}
}

type ArchiveCheck struct {
	Zip         string   `json:"zip"`
	Base        string   `json:"base"`
	Root        string   `json:"root,omitempty"`
	Entries     int      `json:"entries"`
	Identical   int      `json:"identical"`
	Missing     NameList `json:"missing"`    // in the zip, not in the folder
	Different   NameList `json:"different"`  // in both, different bytes
	Unreadable  NameList `json:"unreadable"` // could not be read from the zip or the disk, so not compared
	Extras      NameList `json:"extras"`     // in the folder, not in the zip
	ExtrasKnown bool     `json:"extrasKnown"`
	BytesRead   int64    `json:"bytesRead"`
	ZipInFolder bool     `json:"zipInFolder"` // everything in the zip is in the folder, byte for byte
	FolderInZip bool     `json:"folderInZip"` // everything in the folder is in the zip, byte for byte
	Verdict     string   `json:"verdict"`
	At          int64    `json:"at"` // when it was compared (ms since 1970)
}

type cmpResult int

const (
	cmpSame cmpResult = iota
	cmpDiffers
	cmpZipErr
	cmpDiskErr
)

// verdictText says in a sentence or two how the zip and the folder relate.
func (c *ArchiveCheck) verdictText() string {
	switch {
	case c.ZipInFolder && c.FolderInZip:
		return "Identical: every file in the zip is in the folder, byte for byte, and the folder holds nothing else."
	case c.ZipInFolder && c.ExtrasKnown:
		return fmt.Sprintf("Everything in the zip is in the folder, byte for byte. The folder also holds %d file%s that %s not in the zip.", c.Extras.N, plural(c.Extras.N), isAre(c.Extras.N))
	case c.ZipInFolder:
		return "Everything in the zip is in the folder, byte for byte. What else the folder holds cannot be told, because it shares its place with other files."
	}
	s := fmt.Sprintf("Not everything in the zip is in the folder: %d missing, %d different, %d could not be read.", c.Missing.N, c.Different.N, c.Unreadable.N)
	if c.FolderInZip {
		s += " Everything that is in the folder is in the zip."
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// compareEntry decompresses one entry of the zip and compares it, byte for byte, with the file at diskPath. Go checks the
// entry's CRC-32 as it reads, so a damaged zip shows up as a read error rather than a quiet mismatch.
func compareEntry(zf *zip.File, disk string, b1, b2 []byte, onBytes func(int64)) (cmpResult, error) {
	rc, err := zf.Open()
	if err != nil {
		return cmpZipErr, err
	}
	defer rc.Close()
	f, err := os.Open(disk)
	if err != nil {
		return cmpDiskErr, err
	}
	defer f.Close()
	for {
		n1, e1 := io.ReadFull(rc, b1)
		if e1 != nil && e1 != io.EOF && e1 != io.ErrUnexpectedEOF {
			return cmpZipErr, e1
		}
		n2, e2 := io.ReadFull(f, b2)
		if e2 != nil && e2 != io.EOF && e2 != io.ErrUnexpectedEOF {
			return cmpDiskErr, e2
		}
		if n1 != n2 || !bytes.Equal(b1[:n1], b2[:n2]) {
			return cmpDiffers, nil
		}
		onBytes(int64(n1))
		if n1 < len(b1) {
			return cmpSame, nil
		}
	}
}

// CheckArchive reads the zip and the folder and says exactly how they relate. progress (optional) hears how many bytes
// have been compared of how many there are.
func (a *App) CheckArchive(ctx context.Context, zipPath, base string, strip bool, progress func(done, total int64)) (*ArchiveCheck, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open the zip: %w", err)
	}
	defer zr.Close()
	files, _ := zipFiles(zr)
	top, single := commonTop(files)
	if strip && !single {
		return nil, errors.New("this zip has no single top folder to leave off")
	}
	res := &ArchiveCheck{Zip: zipPath, Base: base, Entries: len(files)}
	res.Root = rootOf(zipPath, base, strip, top, single)
	var total int64
	for _, f := range files {
		total += f.size
	}
	b1, b2 := make([]byte, 1<<20), make([]byte, 1<<20)
	var done int64
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		disk := diskPath(base, f.name, strip, top)
		st, err := os.Lstat(disk)
		switch {
		case err != nil && os.IsNotExist(err):
			res.Missing.add(f.name)
			done += f.size
		case err != nil:
			res.Unreadable.add(f.name + " (" + err.Error() + ")")
			done += f.size
		case !st.Mode().IsRegular():
			res.Different.add(f.name + " (not a regular file in the folder)")
			done += f.size
		case st.Size() != f.size:
			res.Different.add(f.name)
			done += f.size
		default:
			c, err := compareEntry(f.f, disk, b1, b2, func(n int64) {
				done += n
				res.BytesRead += n
				if progress != nil {
					progress(done, total)
				}
			})
			switch c {
			case cmpSame:
				res.Identical++
			case cmpDiffers:
				res.Different.add(f.name)
			default:
				res.Unreadable.add(f.name + " (" + err.Error() + ")")
			}
		}
	}
	if res.Root != "" {
		res.ExtrasKnown = true
		countExtras(res.Root, wantSet(base, files, strip, top), zipPath, &res.Extras)
	}
	res.ZipInFolder = res.Entries > 0 && res.Identical == res.Entries
	res.FolderInZip = res.ExtrasKnown && res.Extras.N == 0 && res.Different.N == 0 && res.Unreadable.N == 0
	res.Verdict = res.verdictText()
	res.At = time.Now().UnixMilli()
	a.rememberCheck(res, strip)
	return res, nil
}

// TrashArchivePart moves the zip, or the folder it was unpacked into, to the Recycle Bin, but only if the comparison made
// right now says that part is wholly contained in the other: the same just-before check every duplicate delete gets.
func (a *App) TrashArchivePart(ctx context.Context, zipPath, base string, strip bool, which string, progress func(done, total int64)) (DelResult, *ArchiveCheck, error) {
	chk, err := a.CheckArchive(ctx, zipPath, base, strip, progress)
	if err != nil {
		return DelResult{}, nil, err
	}
	var target, other string
	switch which {
	case "zip":
		if !chk.ZipInFolder {
			return DelResult{Path: zipPath, Error: "not everything in the zip is in the folder, so the zip is not removed"}, chk, nil
		}
		target, other = zipPath, chk.Root
		if other == "" {
			other = base
		}
	case "folder":
		if chk.Root == "" || !chk.FolderInZip {
			return DelResult{Path: base, Error: "not everything in the folder is in the zip (or that cannot be told), so the folder is not removed"}, chk, nil
		}
		target, other = chk.Root, zipPath
	default:
		return DelResult{}, nil, fmt.Errorf("unknown choice %q", which)
	}
	a.audit(target, 0, "archive-check", "", other)
	return a.TrashPath(target), chk, nil
}

// ---------------------------------------------------------------- remembering what was compared

// ArchiveVerdict is the short form of a comparison: the counts and the sentence, not the lists of names.
type ArchiveVerdict struct {
	At          int64  `json:"at"`
	Entries     int    `json:"entries"`
	Identical   int    `json:"identical"`
	Missing     int    `json:"missing"`
	Different   int    `json:"different"`
	Unreadable  int    `json:"unreadable"`
	Extras      int    `json:"extras"`
	ExtrasKnown bool   `json:"extrasKnown"`
	ZipInFolder bool   `json:"zipInFolder"`
	FolderInZip bool   `json:"folderInZip"`
	Verdict     string `json:"verdict"`
}

func verdictOf(c *ArchiveCheck) ArchiveVerdict {
	return ArchiveVerdict{At: c.At, Entries: c.Entries, Identical: c.Identical, Missing: c.Missing.N, Different: c.Different.N,
		Unreadable: c.Unreadable.N, Extras: c.Extras.N, ExtrasKnown: c.ExtrasKnown, ZipInFolder: c.ZipInFolder, FolderInZip: c.FolderInZip, Verdict: c.Verdict}
}

type arcEntry struct {
	v           ArchiveVerdict
	size, mtime int64
}

type arcCache struct {
	mu sync.Mutex
	m  map[string]arcEntry
}

func arcKey(zipPath, base string, strip bool) string {
	return normKey(zipPath) + "|" + normKey(base) + "|" + strconv.FormatBool(strip)
}

// rememberCheck keeps a comparison so the window, "Compare all" and Claude all see the same result. It is only
// trusted while the zip is the file that was compared (same size and modified time); anything that actually removes
// something compares again first.
func (a *App) rememberCheck(c *ArchiveCheck, strip bool) {
	st, err := os.Stat(c.Zip)
	if err != nil {
		return
	}
	a.arc.mu.Lock()
	if a.arc.m == nil {
		a.arc.m = map[string]arcEntry{}
	}
	a.arc.m[arcKey(c.Zip, c.Base, strip)] = arcEntry{verdictOf(c), st.Size(), st.ModTime().UnixNano()}
	a.arc.mu.Unlock()
}

func (a *App) recalled(zipPath, base string, strip bool) *ArchiveVerdict {
	st, err := os.Stat(zipPath)
	if err != nil {
		return nil
	}
	a.arc.mu.Lock()
	defer a.arc.mu.Unlock()
	e, ok := a.arc.m[arcKey(zipPath, base, strip)]
	if !ok || e.size != st.Size() || e.mtime != st.ModTime().UnixNano() {
		return nil
	}
	v := e.v
	return &v
}

// ---------------------------------------------------------------- comparing many at once

type ArchiveItem struct {
	Zip   string `json:"zip"`
	Base  string `json:"base"`
	Strip bool   `json:"strip"`
}

type arcJob struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// CompareAll compares each of the given zips with its folder in the background, two at a time, telling the window as each
// finishes. What it finds is remembered. It returns at once.
func (a *App) CompareAll(items []ArchiveItem) error {
	a.arcJob.mu.Lock()
	if a.arcJob.running {
		a.arcJob.mu.Unlock()
		return errors.New("a comparison is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.arcJob.running, a.arcJob.cancel = true, cancel
	a.arcJob.mu.Unlock()
	go func() {
		var done atomic.Int32
		work := make(chan ArchiveItem)
		var wg sync.WaitGroup
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for it := range work {
					if ctx.Err() != nil {
						continue
					}
					chk, err := a.CheckArchive(ctx, it.Zip, it.Base, it.Strip, nil)
					switch {
					case err == nil:
						a.hub.Broadcast("archive-result", map[string]any{"zip": it.Zip, "check": verdictOf(chk)})
					case ctx.Err() == nil:
						a.hub.Broadcast("archive-result", map[string]any{"zip": it.Zip, "error": err.Error()})
					}
					a.hub.Broadcast("archive-batch", map[string]any{"done": done.Add(1), "total": len(items), "running": true})
				}
			}()
		}
		for _, it := range items {
			if ctx.Err() != nil {
				break
			}
			work <- it
		}
		close(work)
		wg.Wait()
		a.arcJob.mu.Lock()
		a.arcJob.running, a.arcJob.cancel = false, nil
		a.arcJob.mu.Unlock()
		a.hub.Broadcast("archive-batch", map[string]any{"done": done.Load(), "total": len(items), "running": false, "cancelled": ctx.Err() != nil})
		cancel()
	}()
	return nil
}

// CancelCompare stops a running CompareAll after the zips being read now.
func (a *App) CancelCompare() {
	a.arcJob.mu.Lock()
	if a.arcJob.cancel != nil {
		a.arcJob.cancel()
	}
	a.arcJob.mu.Unlock()
}
