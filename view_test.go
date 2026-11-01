package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func setAttrs(t *testing.T, path string, attrs uint32) {
	t.Helper()
	p, _ := syscall.UTF16PtrFromString(path)
	if err := syscall.SetFileAttributes(p, attrs); err != nil {
		t.Fatal(err)
	}
}

func kinds(items []Item) map[string]Item {
	m := map[string]Item{}
	for _, it := range items {
		m[it.Name] = it
	}
	return m
}

func TestSystemManagedItems(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "normal.bin"), 2*MiB, 1)
	write(t, filepath.Join(root, "pagefile.sys"), 4*MiB, 2)
	write(t, filepath.Join(root, "sysdir", "inside.bin"), 3*MiB, 3)
	write(t, filepath.Join(root, "hiddenonly", "x.bin"), 5*MiB, 4)
	setAttrs(t, filepath.Join(root, "pagefile.sys"), syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_SYSTEM)
	setAttrs(t, filepath.Join(root, "sysdir"), syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_SYSTEM|syscall.FILE_ATTRIBUTE_DIRECTORY)
	setAttrs(t, filepath.Join(root, "hiddenonly"), syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_DIRECTORY) // like AppData
	t.Cleanup(func() {                                                                                             // let the temp dir be removed
		setAttrs(t, filepath.Join(root, "pagefile.sys"), syscall.FILE_ATTRIBUTE_NORMAL)
	})

	tree := scan(t, ScanOpts{Roots: []string{root}, MinNode: MiB, MinFile: MiB}).Tree()
	n, _ := tree.Root.find(root)
	st := n.stats()
	if st.Total != 14*MiB || st.Sys != 7*MiB {
		t.Fatalf("total %d sys %d, want 14 MiB and 7 MiB (pagefile.sys + the whole system folder; hidden-only AppData-style folders are NOT system-managed)", st.Total, st.Sys)
	}

	all := kinds(mustView(t, tree, root, ViewOpts{Depth: 2, Limit: 50}).Children)
	if all["sysdir"].Size != 3*MiB || !all["sysdir"].Sys || !all["pagefile.sys"].Sys || all["hiddenonly"].Sys {
		t.Fatalf("flags wrong: %+v", all)
	}
	hidden := mustView(t, tree, root, ViewOpts{Depth: 2, Limit: 50, HideSys: true})
	h := kinds(hidden.Children)
	if _, ok := h["sysdir"]; ok {
		t.Fatal("a system folder must disappear when system items are hidden")
	}
	if _, ok := h["pagefile.sys"]; ok {
		t.Fatal("a system file must disappear when system items are hidden")
	}
	if hidden.Size != 7*MiB || h["hiddenonly"].Size != 5*MiB || h["normal.bin"].Size != 2*MiB {
		t.Fatalf("hidden view: total %d, %+v", hidden.Size, h)
	}
}

func mustView(t *testing.T, tree *Tree, path string, o ViewOpts) Item {
	t.Helper()
	it, _, ok := tree.View(path, o)
	if !ok {
		t.Fatalf("no view for %q", path)
	}
	return it
}

func TestFilterLanguage(t *testing.T) {
	now := time.Now().UnixNano()
	day := int64(24 * time.Hour)
	type file struct {
		path, name string
		size, age  int64 // age in days
		dup        bool
	}
	files := map[string]file{
		"mov":  {`C:\v\holiday.MOV`, "holiday.MOV", 600 << 20, 5, false},
		"mp4":  {`C:\v\clip.mp4`, "clip.mp4", 50 << 20, 400, true},
		"txt":  {`C:\d\notes.txt`, "notes.txt", 3 << 10, 1, false},
		"node": {`C:\p\node_modules\x\lib.js`, "lib.js", 2 << 20, 90, false},
	}
	cases := []struct {
		expr string
		want []string
	}{
		{"", []string{"mov", "mp4", "txt", "node"}},
		{"*.mov|*.mp4", []string{"mov", "mp4"}},
		{">500MB", []string{"mov"}},
		{"<10KB", []string{"txt"}},
		{">=50mb <1gb", []string{"mov", "mp4"}},
		{"<30d", []string{"mov", "txt"}},
		{">1y", []string{"mp4"}},
		{"type:video", []string{"mov", "mp4"}},
		{"type:code", []string{"node"}},
		{"dup", []string{"mp4"}},
		{"node_modules", []string{"node"}},
		{"-node_modules", []string{"mov", "mp4", "txt"}},
		{"holiday >100MB", []string{"mov"}},
		{`*\v\*`, []string{"mov", "mp4"}}, // a pattern with a backslash matches the whole path
	}
	for _, c := range cases {
		f, bad := parseFilter(c.expr)
		if len(bad) != 0 {
			t.Errorf("%q: unexpected bad terms %v", c.expr, bad)
		}
		var got []string
		for _, k := range []string{"mov", "mp4", "txt", "node"} {
			fl := files[k]
			if f.Match(fl.path, fl.name, fl.size, now-fl.age*day, fl.dup) {
				got = append(got, k)
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%q: got %v, want %v", c.expr, got, c.want)
		}
	}
	if f, bad := parseFilter(">5zz type:nothing ok"); len(bad) != 2 || f == nil {
		t.Errorf("bad terms should be reported and the rest kept: %v %v", bad, f)
	}
	if f, bad := parseFilter("   "); f != nil || bad != nil {
		t.Error("a blank filter is no filter")
	}
}

func TestFilteredView(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "vids", "a.mov"), 3*MiB, 1)
	write(t, filepath.Join(root, "vids", "b.txt"), 2*MiB, 2)
	write(t, filepath.Join(root, "docs", "c.txt"), 2*MiB, 3)
	write(t, filepath.Join(root, "old", "d.bin"), 4*MiB, 4)
	age(t, filepath.Join(root, "old", "d.bin"), 800*24*time.Hour)
	tree := scan(t, ScanOpts{Roots: []string{root}, MinNode: MiB, MinFile: MiB}).Tree()

	view := func(expr string) (Item, map[string]Item, []string) {
		f, bad := parseFilter(expr)
		it := mustView(t, tree, root, ViewOpts{Depth: 3, Limit: 50, Filter: f})
		return it, kinds(it.Children), bad
	}
	it, k, _ := view("*.txt")
	if it.Size != 4*MiB || k["vids"].Size != 2*MiB || k["docs"].Size != 2*MiB || len(k) != 2 {
		t.Fatalf("*.txt: total %d, %+v", it.Size, k)
	}
	if v := k["vids"]; len(v.Children) != 1 || v.Children[0].Name != "b.txt" {
		t.Fatalf("a folder under a filter only holds what matches: %+v", v.Children)
	}
	if it, k, _ = view(">1y"); it.Size != 4*MiB || len(k) != 1 || k["old"].Size != 4*MiB {
		t.Fatalf(">1y: %d %+v", it.Size, k)
	}
	if it, k, _ = view("nothing-matches"); it.Size != 0 || len(k) != 0 {
		t.Fatalf("no match should leave an empty map: %d %+v", it.Size, k)
	}
	if _, bad := parseFilter(">5zz"); len(bad) != 1 {
		t.Fatal("bad term not reported")
	}
}

// A synthetic drive, because a test must not scan C:. The numbers come from the real disk, with a tolerance
// because other programs write to it while the test runs.
func driveTree(t *testing.T, scannedFraction float64) (*Tree, *Node, int64, int64) {
	free, total, err := diskUsage(`C:\`)
	if err != nil {
		t.Skip("no C: drive")
	}
	used := total - free
	root := &Node{}
	drive := &Node{Name: `C:\`, Parent: root, Done: true, Total: int64(float64(used) * scannedFraction), TotalN: 10}
	drive.Own = drive.Total
	root.Dirs = []*Node{drive}
	return &Tree{Root: root, MinFile: MiB, MinNode: MiB, Complete: true, Roots: []string{`C:\`}}, drive, free, used
}

func near(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 300*MiB
}

func TestDriveFreeAndUnaccounted(t *testing.T) {
	tree, drive, free, used := driveTree(t, 0.6)
	it := mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50, ShowFree: true, ShowUnacc: true})
	k := kinds(it.Children)
	if !near(k["Free space"].Size, free) || k["Free space"].Kind != "free" {
		t.Fatalf("free block %d, disk says %d", k["Free space"].Size, free)
	}
	if want := used - drive.Total; !near(k["Unaccounted"].Size, want) || k["Unaccounted"].Kind != "unacc" {
		t.Fatalf("unaccounted block %d, want about %d", k["Unaccounted"].Size, want)
	}
	if !near(it.Size, free+used) { // the drive as a whole adds up to its capacity
		t.Fatalf("the drive block is %d, the capacity is %d", it.Size, free+used)
	}

	// each toggle works on its own
	k = kinds(mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50, ShowFree: true}).Children)
	if _, ok := k["Unaccounted"]; ok || k["Free space"].Size == 0 {
		t.Fatalf("only free should show: %+v", k)
	}
	k = kinds(mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50, ShowUnacc: true}).Children)
	if _, ok := k["Free space"]; ok || k["Unaccounted"].Size == 0 {
		t.Fatalf("only unaccounted should show: %+v", k)
	}
	off := mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50})
	if _, ok := kinds(off.Children)["Free space"]; ok || off.Size != drive.Total {
		t.Fatalf("with both off the view is just what was scanned: %d vs %d", off.Size, drive.Total)
	}

	// in "This PC" the drive block is the whole drive, and its children carry the reconciliation blocks
	pc := mustView(t, tree, "", ViewOpts{Depth: 3, Limit: 50, ShowFree: true, ShowUnacc: true})
	if len(pc.Children) != 1 || !near(pc.Children[0].Size, free+used) {
		t.Fatalf("This PC: %+v", pc.Children)
	}
	if _, ok := kinds(pc.Children[0].Children)["Free space"]; !ok {
		t.Fatal("the nested drive should hold the free-space block")
	}

	// a scan that is still running has no honest "unaccounted": it would just be everything not read yet
	drive.Done = false
	if _, ok := kinds(mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50, ShowFree: true, ShowUnacc: true}).Children)["Unaccounted"]; ok {
		t.Fatal("no unaccounted block while the scan is running")
	}
	// and a filter hides the reconciliation blocks, which are not files
	drive.Done = true
	f, _ := parseFilter("*.zzz")
	if got := mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50, ShowFree: true, ShowUnacc: true, Filter: f}); len(got.Children) != 0 {
		t.Fatalf("filtered view still has %+v", got.Children)
	}
}

func TestRecycleBinGrowsWhenSomethingIsRecycled(t *testing.T) {
	root := &Node{}
	drive := &Node{Name: `C:\`, Parent: root, Done: true}
	docs := &Node{Name: "docs", Parent: drive, Done: true, Own: 10 * MiB, Total: 10 * MiB, OwnN: 1, TotalN: 1,
		Files: []FileEnt{{"big.bin", 10 * MiB, 1, false}}}
	bin := &Node{Name: "$Recycle.Bin", Parent: drive, Done: true, Sys: true, Own: MiB, OwnN: 1, Total: MiB, TotalN: 1, SysTotal: MiB}
	drive.Dirs = []*Node{docs, bin}
	drive.Total, drive.TotalN, drive.SysTotal = 11*MiB, 2, MiB
	root.Dirs = []*Node{drive}
	tree := &Tree{Root: root, MinFile: MiB}

	tree.forgetFile(`C:\docs\big.bin`, 10*MiB)
	tree.bumpRecycle(`C:\docs\big.bin`, 10*MiB, 1)
	if drive.Total != 11*MiB {
		t.Fatalf("recycling moves bytes, it does not free them: the drive is %d, want %d", drive.Total, 11*MiB)
	}
	if docs.Total != 0 || bin.Total != 11*MiB || bin.SysTotal != 11*MiB || drive.SysTotal != 11*MiB {
		t.Fatalf("docs %d, bin %d (sys %d), drive sys %d", docs.Total, bin.Total, bin.SysTotal, drive.SysTotal)
	}
	// with system items hidden, the bin's growth is hidden too
	hidden := mustView(t, tree, `C:\`, ViewOpts{Depth: 2, Limit: 50, HideSys: true})
	if hidden.Size != 0 {
		t.Fatalf("hiding system items should leave nothing here, got %d", hidden.Size)
	}
}

func TestDeepNestingStaysWithinItsBudget(t *testing.T) {
	root := t.TempDir()
	for a := 0; a < 8; a++ {
		for b := 0; b < 8; b++ {
			for c := 0; c < 4; c++ {
				write(t, filepath.Join(root, fmt.Sprintf("a%d", a), fmt.Sprintf("b%d", b), fmt.Sprintf("c%d", c), "f.bin"), MiB+1, byte(a+b+c))
			}
		}
	}
	tree := scan(t, ScanOpts{Roots: []string{root}, MinNode: MiB, MinFile: MiB}).Tree()
	count := func(it Item) int {
		var walk func(Item) int
		walk = func(x Item) int {
			n := 1
			for _, c := range x.Children {
				n += walk(c)
			}
			return n
		}
		return walk(it)
	}
	deep := mustView(t, tree, root, ViewOpts{Depth: 4, Limit: 60})
	if n := count(deep); n < 100 {
		t.Fatalf("a deep view should nest several levels, only %d items", n)
	}
	small := mustView(t, tree, root, ViewOpts{Depth: 4, Limit: 10, Budget: 30})
	if n := count(small); n > 80 {
		t.Fatalf("the budget should cap the answer, got %d items", n)
	}
	flat := mustView(t, tree, root, ViewOpts{Depth: 1, Limit: 60})
	for _, c := range flat.Children {
		if len(c.Children) != 0 {
			t.Fatal("depth 1 means no nesting")
		}
	}
}

func TestDuplicateExcludeCoversEverythingBelowAFolder(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "ok", "f.bin"), 2*MiB, 1)
	write(t, filepath.Join(root, "skipme", "deep", "deeper", "f.bin"), 2*MiB, 1)
	write(t, filepath.Join(root, "other", "f.bin"), 2*MiB, 1)
	sc := scan(t, ScanOpts{Roots: []string{root}, MinDup: MiB, DupExcludes: []string{"skipme"}})
	var got []string
	for _, c := range sc.Candidates() {
		got = append(got, rel(root, c.Path))
	}
	if len(got) != 2 || strings.Contains(strings.Join(got, "|"), "skipme") {
		t.Fatalf("candidates %v: a folder-name rule must cover all files below it", got)
	}
	if sc.Files.Load() != 3 {
		t.Fatalf("an excluded-from-duplicates folder is still counted on the map: %d files", sc.Files.Load())
	}
}

func TestSettingsMigration(t *testing.T) {
	s := Settings{Excludes: []string{"$Recycle.Bin", "node_modules", "System Volume Information"}, DupExcludes: []string{`C:\Windows`}}
	migrateSettings(&s)
	if len(s.Excludes) != 1 || s.Excludes[0] != "node_modules" || s.Schema != 2 {
		t.Fatalf("excludes %v schema %d: the old defaults hid the Recycle Bin and must go, user choices stay", s.Excludes, s.Schema)
	}
	if len(s.DupExcludes) != 2 || s.DupExcludes[1] != "$Recycle.Bin" {
		t.Fatalf("dup excludes %v", s.DupExcludes)
	}
	again := s
	migrateSettings(&again)
	if len(again.DupExcludes) != 2 || len(again.Excludes) != 1 {
		t.Fatal("migration must be idempotent: a user who adds the bin back later keeps it")
	}
	if d := defaultSettings(); d.Schema != 2 || len(d.Excludes) != 0 {
		t.Fatalf("defaults: %+v", d)
	}
}

func TestProgressNeedsAWholeDriveScan(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "f.bin"), 100, 1)
	scanApp(t, a, root)
	if a.Status().Expected != 0 {
		t.Fatalf("a folder scan has no known size to measure against, got %d", a.Status().Expected)
	}
	free, total, err := diskUsage(`C:\`)
	if err != nil {
		t.Skip("no C: drive")
	}
	// a whole-drive scan would expect "used" bytes; check the same arithmetic the scan uses without scanning C:
	if want := total - free; want <= 0 || !isDriveRoot(`C:\`) || isDriveRoot(`C:\Users`) {
		t.Fatalf("drive-root detection is off: used %d", want)
	}
	_ = os.Getenv
}

func TestFoldersKnowWhatTheyAreMadeOf(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "vids", "a.mp4"), 3*MiB, 1)
	write(t, filepath.Join(root, "vids", "small.txt"), 100, 2) // too small to list, still counted
	write(t, filepath.Join(root, "code", "x.go"), 2*MiB, 3)
	write(t, filepath.Join(root, "mixed", "a.mp4"), MiB, 4)
	write(t, filepath.Join(root, "mixed", "b.zip"), MiB, 5)
	write(t, filepath.Join(root, "outer", "inner", "movie.MKV"), 2*MiB, 6)
	tree := scan(t, ScanOpts{Roots: []string{root}, MinNode: MiB, MinFile: MiB}).Tree()
	it := mustView(t, tree, root, ViewOpts{Depth: 3, Limit: 50})
	k := kinds(it.Children)
	if k["vids"].Dom != "video" || k["vids"].Pure < 0.99 {
		t.Fatalf("vids: %+v", k["vids"])
	}
	if k["code"].Dom != "code" || k["code"].Pure != 1 {
		t.Fatalf("code: %+v", k["code"])
	}
	if p := k["mixed"].Pure; p < 0.49 || p > 0.51 {
		t.Fatalf("a half-and-half folder should be about 0.5 pure, got %v (%s)", p, k["mixed"].Dom)
	}
	if k["outer"].Dom != "video" { // a class is folded up through the folders in between
		t.Fatalf("outer: %+v", k["outer"])
	}
	vids := kinds(k["vids"].Children)
	if vids["a.mp4"].Type != "video" {
		t.Fatalf("file type: %+v", vids["a.mp4"])
	}
	if typeIdxOf("Photo.JPG") != typeIndex["image"] || typeIdxOf("noextension") != 0 || typeIdxOf("x.zip") != typeIndex["archive"] {
		t.Fatal("typeIdxOf")
	}
	if n, s := dominant([8]int64{}); n != "" || s != 0 {
		t.Fatal("an empty folder has no dominant class")
	}
}
