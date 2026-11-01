package main

// App ties everything together: the space map, the duplicate groups, settings, and the live event stream.
// All state lives here; the HTTP layer only translates requests into calls on App.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// version is set at release time (go build -ldflags "-X main.version=1.2.3"); this is the development value.
var version = "1.7.0"

type Settings struct {
	Schema      int            `json:"schema"`
	Roots       []string       `json:"roots"`
	Excludes    []string       `json:"excludes"`
	DupExcludes []string       `json:"dupExcludes"`
	MinDup      int64          `json:"minDup"`
	FindDupes   bool           `json:"findDupes"`
	Workers     int            `json:"workers"`
	Prefer      []string       `json:"prefer"`
	Prefs       map[string]any `json:"prefs"` // how the window likes things (toggles, side panel): kept here because the window's own browser profile is thrown away on exit
}

func defaultSettings() Settings {
	s := Settings{
		Schema:      2,
		Roots:       fixedDrives(),
		Excludes:    []string{},
		DupExcludes: []string{"$Recycle.Bin"}, // shown on the map, but what is in the bin is not a duplicate to offer
		MinDup:      1 << 20,
		FindDupes:   true,
		Workers:     4,
	}
	for _, env := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)"} {
		if v := os.Getenv(env); v != "" {
			s.DupExcludes = append(s.DupExcludes, v)
		}
	}
	return s
}

// migrateSettings brings settings saved by an older version up to date: the Recycle Bin and System Volume Information
// used to be excluded from the map by default, which hid space that is really used.
func migrateSettings(s *Settings) {
	if s.Schema >= 2 {
		return
	}
	keep := []string{}
	for _, e := range s.Excludes {
		if l := strings.ToLower(strings.TrimSpace(e)); l != "$recycle.bin" && l != "system volume information" {
			keep = append(keep, e)
		}
	}
	s.Excludes = keep
	has := false
	for _, e := range s.DupExcludes {
		if strings.EqualFold(strings.TrimSpace(e), "$Recycle.Bin") {
			has = true
		}
	}
	if !has {
		s.DupExcludes = append(s.DupExcludes, "$Recycle.Bin")
	}
	s.Schema = 2
}

type Status struct {
	Phase          string  `json:"phase"` // idle | scanning | hashing | done | cancelled
	Source         string  `json:"source,omitempty"`
	Message        string  `json:"message,omitempty"`
	Files          int64   `json:"files"`
	Dirs           int64   `json:"dirs"`
	Bytes          int64   `json:"bytes"`
	Expected       int64   `json:"expected,omitempty"` // bytes the disk says are used: what "100%" means for a whole-drive scan
	Errors         int64   `json:"errors"`
	Current        string  `json:"current,omitempty"`
	SizeGroups     int64   `json:"sizeGroups"`
	SizeGroupsDone int64   `json:"sizeGroupsDone"`
	BytesHashed    int64   `json:"bytesHashed"`
	Groups         int     `json:"groups"`
	Reclaimable    int64   `json:"reclaimable"`
	Started        int64   `json:"started"` // unix ms
	Elapsed        float64 `json:"elapsed"`
	Partial        bool    `json:"partial,omitempty"`
	Imported       string  `json:"imported,omitempty"`
}

type Disk struct {
	Drive string `json:"drive"`
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
}

type ref struct {
	g *Group
	c *Copy
}

type App struct {
	dataDir string
	token   string
	hub     *Hub

	mu       sync.RWMutex // settings, categories, tree pointer, groups
	settings Settings
	cats     []Category
	cls      *Classifier
	catRev   int64
	tree     *Tree
	treeRev  atomic.Int64
	groups   []*Group
	dirs     map[string]*dirEnt // what the duplicate search saw in each folder (folders.go); nil without a scan
	index    map[string]ref
	rev      int64
	epoch    int64
	opts     ScanOpts // the options of the last scan, reused to refresh a folder

	smu     sync.Mutex // scan status
	status  Status
	running bool
	cancel  context.CancelFunc
	dup     *DupProgress
	errs    []ScanError

	delMu  sync.Mutex // one delete at a time
	live   liveState  // the folder being followed live (watch.go)
	arc    arcCache   // what zips were last compared with (archives.go)
	arcJob arcJob     // "Compare all", while it runs

	cl   claudeState
	chat *Chat
	port int // where the window (and the chat panel's Claude) reach this app
}

var errBusy = errors.New("a scan is already running")

func newApp(dataDir, token string) *App {
	a := &App{dataDir: dataDir, token: token, hub: newHub(), index: map[string]ref{}, status: Status{Phase: "idle"}}
	a.cl.pending = map[string]*Approval{}
	a.chat = newChat(a)
	a.settings = defaultSettings()
	if b, err := os.ReadFile(filepath.Join(dataDir, "settings.json")); err == nil {
		var s Settings
		if json.Unmarshal(b, &s) == nil && len(s.Roots) > 0 {
			migrateSettings(&s)
			a.settings = s
		}
	}
	a.cats = loadCategories(filepath.Join(dataDir, "categories.json"))
	a.cls, _ = newClassifier(a.cats, "")
	if a.cls == nil { // cannot happen for validated categories, but never run without a classifier
		a.cats = defaultCategories()
		a.cls, _ = newClassifier(a.cats, "")
	}
	return a
}

func (a *App) saveSettings() {
	a.mu.RLock()
	b, _ := json.MarshalIndent(a.settings, "", "  ")
	a.mu.RUnlock()
	os.WriteFile(filepath.Join(a.dataDir, "settings.json"), b, 0o644)
}

// ---------------------------------------------------------------- scanning

func (a *App) setStatus(f func(*Status)) Status {
	a.smu.Lock()
	defer a.smu.Unlock()
	f(&a.status)
	return a.status
}

func (a *App) Status() Status {
	a.smu.Lock()
	defer a.smu.Unlock()
	return a.status
}

// StartScan begins reading the disk in the background and returns at once, and remembers the choices for next time.
func (a *App) StartScan(cfg Settings) error { return a.startScan(cfg, true) }

// startScan with remember=false leaves the saved settings alone (used when Claude starts a scan).
func (a *App) startScan(cfg Settings, remember bool) error {
	cfg.Roots = normalizeRoots(cfg.Roots)
	if len(cfg.Roots) == 0 {
		return errors.New("choose at least one folder or drive to scan")
	}
	if cfg.MinDup <= 0 {
		cfg.MinDup = 1 << 20
	}
	if cfg.Workers < 1 || cfg.Workers > 16 {
		cfg.Workers = 4
	}
	a.smu.Lock()
	if a.running {
		a.smu.Unlock()
		return errBusy
	}
	a.running = true
	a.smu.Unlock()

	var expected int64 // only a scan of whole drives has a known size to measure progress against
	for _, r := range cfg.Roots {
		free, total, err := diskUsage(r)
		if !isDriveRoot(r) || err != nil {
			expected = 0
			break
		}
		expected += total - free
	}
	ctx, cancel := context.WithCancel(context.Background())
	opts := ScanOpts{Roots: cfg.Roots, Excludes: cfg.Excludes, DupExcludes: cfg.DupExcludes,
		MinNode: 1 << 20, MinFile: 1 << 20, MaxFiles: 300, Workers: 16}
	if cfg.FindDupes {
		opts.MinDup = cfg.MinDup
	}
	sc := newScanner(opts)

	a.mu.Lock()
	cfg.Prefer = a.settings.Prefer
	if remember {
		cfg.Prefs, cfg.Schema = a.settings.Prefs, 2 // the scan dialog does not send these: keep them
		a.settings = cfg
	}
	a.tree = sc.Tree()
	a.opts = opts
	a.resetGroupsLocked()
	a.mu.Unlock()
	if remember {
		a.saveSettings()
	}
	a.treeRev.Add(1)
	a.smu.Lock()
	a.cancel, a.dup, a.errs = cancel, nil, nil
	a.smu.Unlock()
	a.publishGroups()
	a.hub.Broadcast("tree", map[string]any{"rev": a.treeRev.Load()})

	go a.runScan(ctx, cancel, cfg, sc, expected)
	return nil
}

func (a *App) resetGroupsLocked() {
	a.groups, a.index, a.dirs = nil, map[string]ref{}, nil
	a.epoch++
	a.rev++
}

func (a *App) Cancel() {
	a.smu.Lock()
	c := a.cancel
	a.smu.Unlock()
	if c != nil {
		c()
	}
}

func (a *App) runScan(ctx context.Context, cancel context.CancelFunc, cfg Settings, sc *Scanner, expected int64) {
	defer cancel()
	start := time.Now()
	a.setStatus(func(s *Status) {
		*s = Status{Phase: "scanning", Source: "scan", Started: start.UnixMilli(), Expected: expected}
	})

	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		var n int
		var lastRev int64 = -1
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				n++
				a.tick(sc, start)
				if n%4 == 0 {
					a.treeRev.Add(1)
					a.hub.Broadcast("tree", map[string]any{"rev": a.treeRev.Load()})
				}
				a.mu.RLock()
				r := a.rev
				a.mu.RUnlock()
				if r != lastRev {
					lastRev = r
					a.publishGroups()
				}
			}
		}
	}()

	sc.Run(ctx)
	if cfg.FindDupes {
		idx := buildDirIndex(sc.Candidates())
		a.mu.Lock()
		a.dirs = idx
		a.mu.Unlock()
	}
	if ctx.Err() == nil && cfg.FindDupes {
		a.setStatus(func(s *Status) { s.Phase = "hashing" })
		cache := openHashCache(filepath.Join(a.dataDir, "hashcache.tsv"))
		dp := &DupProgress{}
		a.smu.Lock()
		a.dup = dp
		a.smu.Unlock()
		var errMu sync.Mutex
		var dupErrs []ScanError
		findDuplicates(ctx, sc.Candidates(), cache, cfg.Workers, cfg.Prefer, dp,
			func(p string, err error) {
				errMu.Lock()
				if len(dupErrs) < 200 {
					dupErrs = append(dupErrs, ScanError{p, err.Error()})
				}
				errMu.Unlock()
			}, a.addGroup)
		cache.close()
		errMu.Lock()
		sc.errMu.Lock()
		sc.errList = append(sc.errList, dupErrs...)
		sc.errMu.Unlock()
		errMu.Unlock()
	}
	close(stop)
	<-stopped

	phase := "done"
	if ctx.Err() != nil {
		phase = "cancelled"
	}
	a.tick(sc, start) // the final counters, while the phase still says what was running
	a.smu.Lock()      // "not running" and "done" must change together, or a caller can see a finished scan still hashing
	a.running, a.cancel, a.errs = false, nil, sc.ErrorList()
	a.status.Phase, a.status.Current, a.status.Partial = phase, "", ctx.Err() != nil
	st := a.status
	a.smu.Unlock()
	a.treeRev.Add(1)
	a.hub.Broadcast("tree", map[string]any{"rev": a.treeRev.Load()})
	a.publishGroups()
	a.hub.Broadcast("status", st)
}

func (a *App) tick(sc *Scanner, start time.Time) {
	groups, reclaim := a.liveStats()
	a.smu.Lock()
	s := &a.status
	s.Files, s.Dirs, s.Bytes, s.Errors = sc.Files.Load(), sc.Dirs.Load(), sc.Bytes.Load(), sc.Errs.Load()
	s.Elapsed = time.Since(start).Seconds()
	s.Groups, s.Reclaimable = groups, reclaim
	if a.dup != nil {
		s.SizeGroups, s.SizeGroupsDone, s.BytesHashed = a.dup.SizeGroups.Load(), a.dup.SizeGroupsDone.Load(), a.dup.BytesHashed.Load()
		s.Current = a.dup.Current()
	} else {
		s.Current = sc.Current()
	}
	st := *s
	a.smu.Unlock()
	a.hub.Broadcast("status", st)
}

// ---------------------------------------------------------------- groups

func (a *App) addGroup(g *Group) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rev++
	g.Rev = a.rev
	g.ID = len(a.groups) + 1
	for _, c := range g.Copies {
		c.Cat, c.Loc = a.cls.Category(c.Path), location(c.Path)
		a.index[normKey(c.Path)] = ref{g, c}
	}
	a.groups = append(a.groups, g)
}

// liveStats counts the groups that still have two or more copies on disk, and the bytes that could be reclaimed.
func (a *App) liveStats() (groups int, reclaim int64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, g := range a.groups {
		if n := len(g.live()); n >= 2 {
			groups++
			reclaim += g.Size * int64(n-1)
		}
	}
	return
}

func (a *App) publishGroups() {
	n, reclaim := a.liveStats()
	a.mu.RLock()
	msg := map[string]any{"rev": a.rev, "epoch": a.epoch, "count": n, "reclaimable": reclaim}
	a.mu.RUnlock()
	a.hub.Broadcast("groups", msg)
}

type CopyJSON struct {
	Path  string `json:"path"`
	MTime string `json:"mtime"`
	Keep  bool   `json:"keep"`
	Links int    `json:"links,omitempty"`
	Cat   int    `json:"cat"`
	Loc   string `json:"loc"`
	Gone  bool   `json:"gone,omitempty"`
}

type GroupJSON struct {
	ID     int        `json:"id"`
	Size   int64      `json:"size"`
	Rev    int64      `json:"rev"`
	Copies []CopyJSON `json:"copies"`
}

type GroupsResp struct {
	Rev    int64       `json:"rev"`
	Epoch  int64       `json:"epoch"`
	Groups []GroupJSON `json:"groups"`
}

// GroupsSince returns the groups that changed after rev. If the caller is on an older epoch (a new scan or import
// replaced everything) it gets all of them.
func (a *App) GroupsSince(since, epoch int64) GroupsResp {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if epoch != a.epoch {
		since = 0
	}
	resp := GroupsResp{Rev: a.rev, Epoch: a.epoch, Groups: []GroupJSON{}}
	for _, g := range a.groups {
		if g.Rev <= since {
			continue
		}
		gj := GroupJSON{ID: g.ID, Size: g.Size, Rev: g.Rev}
		for _, c := range g.Copies {
			gj.Copies = append(gj.Copies, CopyJSON{c.Path, fmtTime(c.MTime), c.Keep, len(c.Links), c.Cat, c.Loc, c.Gone})
		}
		resp.Groups = append(resp.Groups, gj)
	}
	return resp
}

// isDup says whether a file is currently one of two or more identical copies, for highlighting on the map.
func (a *App) isDup(path string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	r, ok := a.index[normKey(path)]
	return ok && !r.c.Gone && len(r.g.live()) >= 2
}

// ---------------------------------------------------------------- categories

func (a *App) Categories() ([]Category, int64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Category(nil), a.cats...), a.catRev
}

func (a *App) SetCategories(cats []Category) error {
	if err := validateCategories(cats); err != nil {
		return err
	}
	cls, err := newClassifier(cats, "")
	if err != nil {
		return err
	}
	if err := saveCategories(filepath.Join(a.dataDir, "categories.json"), cats); err != nil {
		return err
	}
	a.mu.Lock()
	a.cats, a.cls = cats, cls
	a.catRev++
	for _, g := range a.groups { // every copy may land in another category now: everything counts as changed
		for _, c := range g.Copies {
			c.Cat = cls.Category(c.Path)
		}
		a.rev++
		g.Rev = a.rev
	}
	rev := a.catRev
	a.mu.Unlock()
	a.hub.Broadcast("categories", map[string]any{"rev": rev})
	a.publishGroups()
	return nil
}

// ---------------------------------------------------------------- state for a freshly connected window

type StateResp struct {
	Version  string     `json:"version"`
	Status   Status     `json:"status"`
	Disk     []Disk     `json:"disk"`
	Settings Settings   `json:"settings"`
	Cats     []Category `json:"cats"`
	CatRev   int64      `json:"catRev"`
	Rev      int64      `json:"rev"`
	Epoch    int64      `json:"epoch"`
	TreeRev  int64      `json:"treeRev"`
	HaveTree bool       `json:"haveTree"`
	Drives   []string   `json:"drives"`
	DataDir  string     `json:"dataDir"`
	User     string     `json:"user"`

	Pending       []Approval   `json:"pending"`       // Claude's requests waiting for the user's answer
	Suggestions   []Suggestion `json:"suggestions"`   // Claude's suggestions not yet on the user's list
	ChatAvailable bool         `json:"chatAvailable"` // the claude command was found
	Trusted       bool         `json:"trusted"`
}

func (a *App) State() StateResp {
	a.mu.RLock()
	r := StateResp{Version: version, Settings: a.settings, Cats: a.cats, CatRev: a.catRev, Rev: a.rev, Epoch: a.epoch,
		TreeRev: a.treeRev.Load(), HaveTree: a.tree != nil, DataDir: a.dataDir, User: filepath.Base(os.Getenv("USERPROFILE"))}
	a.mu.RUnlock()
	r.Status = a.Status()
	r.Pending, r.Suggestions = a.pendingApprovals(), a.pendingSuggestions()
	r.ChatAvailable = findCLI() != ""
	a.cl.mu.Lock()
	r.Trusted = a.cl.trusted
	a.cl.mu.Unlock()
	r.Disk = diskStats()
	r.Drives = fixedDrives()
	return r
}

func diskStats() []Disk {
	var out []Disk
	for _, d := range fixedDrives() {
		if free, total, err := diskUsage(d); err == nil {
			out = append(out, Disk{d[:2], free, total})
		}
	}
	return out
}
