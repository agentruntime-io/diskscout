package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write creates a file of the given size whose content is determined by seed.
func write(t *testing.T, path string, size int, seed byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*31) + seed
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func age(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	ts := time.Now().Add(-ago)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, opts ScanOpts) *Scanner {
	t.Helper()
	sc := newScanner(opts)
	sc.Run(context.Background())
	return sc
}

const MiB = 1 << 20

func TestScanTotalsAndPruning(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "big1.bin"), 3*MiB, 1)
	write(t, filepath.Join(root, "a", "small.txt"), 10, 2)
	write(t, filepath.Join(root, "a", "sub", "big2.bin"), 2*MiB, 3)
	for i := 0; i < 5; i++ {
		write(t, filepath.Join(root, "b", "t"+string(rune('0'+i))), 1024, 4)
	}
	os.MkdirAll(filepath.Join(root, "c"), 0o755)

	sc := scan(t, ScanOpts{Roots: []string{root}, MinNode: MiB, MinFile: MiB})
	tree := sc.Tree()
	want := int64(3*MiB + 10 + 2*MiB + 5*1024)
	r, exact := tree.Root.find(root)
	if !exact || r == nil {
		t.Fatalf("scan root not found")
	}
	if total, n, done := r.sums(); total != want || n != 8 || !done {
		t.Fatalf("root = %d bytes %d files done=%v, want %d bytes 8 files", total, n, done, want)
	}
	if sc.Files.Load() != 8 || sc.Bytes.Load() != want {
		t.Fatalf("counters = %d files %d bytes", sc.Files.Load(), sc.Bytes.Load())
	}
	// b (5 KiB) is folded into the parent's "small folders"; a and a\sub stay
	if len(r.Dirs) != 1 || r.Dirs[0].Name != "a" || r.Rest != 5*1024 {
		t.Fatalf("pruning wrong: %d dirs, rest %d", len(r.Dirs), r.Rest)
	}
	if n, ok := tree.Root.find(filepath.Join(root, "a", "sub")); !ok || n.Name != "sub" {
		t.Fatalf("a\\sub not found")
	}
	// a pruned folder resolves to the nearest folder that still exists
	if n, ok := tree.Root.find(filepath.Join(root, "b")); ok || n != r {
		t.Fatalf("pruned folder should resolve to its parent, got exact=%v", ok)
	}

	it, crumbs, ok := tree.View(root, ViewOpts{Depth: 2, Limit: 60})
	if !ok || it.Size != want || len(crumbs) != 2 {
		t.Fatalf("view: ok=%v size=%d crumbs=%v", ok, it.Size, crumbs)
	}
	var a *Item
	var small int64
	for i := range it.Children {
		switch it.Children[i].Kind {
		case "dir":
			a = &it.Children[i]
		case "small":
			small += it.Children[i].Size
		}
	}
	if a == nil || a.Size != int64(5*MiB+10) || small != 5*1024 {
		t.Fatalf("children wrong: %+v small=%d", it.Children, small)
	}
	names := map[string]bool{}
	for _, c := range a.Children {
		names[c.Name] = true
	}
	if !names["big1.bin"] || !names["sub"] || len(a.Children) != 3 { // big1, sub, and the 10-byte file lump
		t.Fatalf("nested children wrong: %+v", a.Children)
	}
}

func TestScanExcludes(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "keep", "x.bin"), 100, 1)
	write(t, filepath.Join(root, "node_modules", "y.bin"), 100, 1)
	write(t, filepath.Join(root, "deep", "node_modules", "z.bin"), 100, 1)
	write(t, filepath.Join(root, "skip", "this", "w.bin"), 100, 1)
	write(t, filepath.Join(root, "note.tmp"), 100, 1)
	sc := scan(t, ScanOpts{Roots: []string{root}, Excludes: []string{"node_modules", filepath.Join(root, "skip"), "*.tmp"}})
	if got := sc.Files.Load(); got != 1 {
		t.Fatalf("scanned %d files, want only keep\\x.bin", got)
	}
}

func TestScanSkipsJunctions(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	write(t, filepath.Join(other, "real.bin"), 1000, 1)
	write(t, filepath.Join(root, "own.bin"), 10, 1)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "link"), other).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v %s", err, out)
	}
	sc := scan(t, ScanOpts{Roots: []string{root}})
	if sc.Files.Load() != 1 || sc.Bytes.Load() != 10 {
		t.Fatalf("followed a junction: %d files %d bytes", sc.Files.Load(), sc.Bytes.Load())
	}
}

func TestScanCancel(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		write(t, filepath.Join(root, fmt.Sprintf("d%d", i%7), fmt.Sprintf("f%d", i)), 10, 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sc := newScanner(ScanOpts{Roots: []string{root}})
	done := make(chan struct{})
	go func() { sc.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled scan did not stop")
	}
	if sc.Tree().Complete {
		t.Fatal("a cancelled scan must not claim to be complete")
	}
}

func TestForgetAndRescan(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "one.bin"), 2*MiB, 1)
	write(t, filepath.Join(root, "a", "two.bin"), 3*MiB, 2)
	opts := ScanOpts{Roots: []string{root}, MinNode: MiB, MinFile: MiB}
	tree := scan(t, opts).Tree()
	total := func() int64 { n, _ := tree.Root.find(root); s, _, _ := n.sums(); return s }
	if total() != 5*MiB {
		t.Fatalf("start total %d", total())
	}
	tree.forgetFile(filepath.Join(root, "a", "one.bin"), 2*MiB)
	if total() != 3*MiB {
		t.Fatalf("after forgetting a file: %d", total())
	}
	if size, _, ok := tree.forgetDir(filepath.Join(root, "a")); !ok || size != 3*MiB || total() != 0 {
		t.Fatalf("forgetDir: size=%d ok=%v total=%d", size, ok, total())
	}

	// a change made outside the app shows up after a rescan of just that folder
	tree = scan(t, opts).Tree()
	os.Remove(filepath.Join(root, "a", "two.bin"))
	if delta, ok := tree.rescan(context.Background(), filepath.Join(root, "a"), opts); !ok || delta != -3*MiB {
		t.Fatalf("rescan delta=%d ok=%v", delta, ok)
	}
	n, _ := tree.Root.find(root)
	if s, _, _ := n.sums(); s != 2*MiB {
		t.Fatalf("total after rescan = %d", s)
	}
	if a, ok := tree.Root.find(filepath.Join(root, "a")); !ok || a.Name != "a" {
		t.Fatalf("the rescanned folder lost its name")
	}
}

// ---------------------------------------------------------------- duplicates

func candidates(t *testing.T, root string) []Candidate {
	t.Helper()
	return scan(t, ScanOpts{Roots: []string{root}, MinDup: 1}).Candidates()
}

func groupsOf(t *testing.T, root string, cache *hashCache, prefer []string) ([]*Group, *DupProgress) {
	t.Helper()
	var out []*Group
	p := &DupProgress{}
	findDuplicates(context.Background(), candidates(t, root), cache, 4, prefer, p,
		func(path string, err error) { t.Errorf("unexpected error on %s: %v", path, err) },
		func(g *Group) { out = append(out, g) })
	return out, p
}

func rel(root, p string) string { return strings.TrimPrefix(p, root+`\`) }

func TestFindDuplicates(t *testing.T) {
	root := t.TempDir()
	big := 300 * 1024
	write(t, filepath.Join(root, "a1.bin"), big, 1)
	write(t, filepath.Join(root, "sub", "a2.bin"), big, 1)
	// same size, same first and last 64 KB, different in the middle: only a full read tells them apart
	write(t, filepath.Join(root, "b.bin"), big, 1)
	f, _ := os.OpenFile(filepath.Join(root, "b.bin"), os.O_RDWR, 0)
	f.WriteAt([]byte{0xFF, 0xEE}, int64(big/2))
	f.Close()
	// small files go straight to a full hash
	write(t, filepath.Join(root, "s1.txt"), 100, 9)
	write(t, filepath.Join(root, "s2.txt"), 100, 9)
	write(t, filepath.Join(root, "s3.txt"), 100, 8)
	// a hardlinked pair is ONE file: nothing to reclaim
	write(t, filepath.Join(root, "h1.bin"), 5000, 5)
	if err := os.Link(filepath.Join(root, "h1.bin"), filepath.Join(root, "h2.bin")); err != nil {
		t.Skipf("no hardlinks here: %v", err)
	}
	// a hardlinked name next to a real copy: a group of two, one of them with a second name
	write(t, filepath.Join(root, "e1.bin"), 7000, 6)
	write(t, filepath.Join(root, "e2.bin"), 7000, 6)
	os.Link(filepath.Join(root, "e1.bin"), filepath.Join(root, "e1-link.bin"))

	groups, _ := groupsOf(t, root, openHashCache(""), nil)
	got := map[string]bool{}
	for _, g := range groups {
		var names []string
		for _, c := range g.Copies {
			names = append(names, rel(root, c.Path))
		}
		got[strings.Join(sorted(names), "|")] = true
		if g.Size == 7000 {
			links := 0
			for _, c := range g.Copies {
				links += len(c.Links)
			}
			if links != 1 {
				t.Errorf("the hardlinked name should be listed as a link of one copy, got %d links", links)
			}
		}
	}
	want := []string{`a1.bin|sub\a2.bin`, `s1.txt|s2.txt`}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing group %q; got %v", w, got)
		}
	}
	if len(groups) != 3 { // a1+a2, s1+s2, and e1(+link)+e2
		t.Errorf("want 3 groups, got %d: %v", len(groups), got)
	}
	for k := range got {
		if strings.Contains(k, "h1.bin") || strings.Contains(k, "b.bin") || strings.Contains(k, "s3.txt") {
			t.Errorf("wrongly grouped: %s", k)
		}
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestHashCacheSkipsRereading(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "x1.bin"), 400*1024, 1)
	write(t, filepath.Join(root, "x2.bin"), 400*1024, 1)
	cachePath := filepath.Join(t.TempDir(), "cache.tsv")

	c1 := openHashCache(cachePath)
	_, p1 := groupsOf(t, root, c1, nil)
	c1.close()
	if p1.BytesHashed.Load() == 0 {
		t.Fatal("the first run must read the files")
	}
	c2 := openHashCache(cachePath)
	g2, p2 := groupsOf(t, root, c2, nil)
	if len(g2) != 1 || p2.BytesHashed.Load() != 0 || c2.Hits.Load() == 0 {
		t.Fatalf("second run: %d groups, %d bytes read, %d cache hits", len(g2), p2.BytesHashed.Load(), c2.Hits.Load())
	}
	// a changed file is read again
	write(t, filepath.Join(root, "x2.bin"), 400*1024, 2)
	c2.close()
	c3 := openHashCache(cachePath)
	defer c3.close()
	g3, _ := groupsOf(t, root, c3, nil)
	if len(g3) != 0 {
		t.Fatalf("changed content is still reported as a duplicate")
	}
}

func TestKeeperChoice(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "new", "f.bin"), 1000, 1)
	write(t, filepath.Join(root, "old", "f.bin"), 1000, 1)
	write(t, filepath.Join(root, "pref", "longer-name.bin"), 1000, 1)
	age(t, filepath.Join(root, "old", "f.bin"), 48*time.Hour)

	g, _ := groupsOf(t, root, openHashCache(""), nil)
	if len(g) != 1 || !strings.HasSuffix(g[0].Copies[0].Path, `old\f.bin`) || !g[0].Copies[0].Keep {
		t.Fatalf("the oldest copy should be the suggested keeper first: %+v", g[0].Copies[0])
	}
	if keeps := countKeeps(g[0]); keeps != 1 {
		t.Fatalf("exactly one KEEP expected, got %d", keeps)
	}
	g, _ = groupsOf(t, root, openHashCache(""), []string{filepath.Join(root, "pref")})
	if !strings.Contains(g[0].Copies[0].Path, `\pref\`) {
		t.Fatalf("a preferred folder should win over age: %s", g[0].Copies[0].Path)
	}
}

func countKeeps(g *Group) int {
	n := 0
	for _, c := range g.Copies {
		if c.Keep {
			n++
		}
	}
	return n
}

func TestFindDuplicatesCancelled(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.bin"), 400*1024, 1)
	write(t, filepath.Join(root, "b.bin"), 400*1024, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var n int
	findDuplicates(ctx, candidates(t, root), openHashCache(""), 2, nil, &DupProgress{},
		func(string, error) {}, func(*Group) { n++ })
	if n != 0 {
		t.Fatalf("a cancelled run reported %d groups", n)
	}
}

// ---------------------------------------------------------------- categories and paths

func TestDefaultCategories(t *testing.T) {
	cls, err := newClassifier(defaultCategories(), "sam")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`C:\Users\Alice\Documents\x.pdf`:                      "Other user accounts",
		`C:\Users\Sam\Documents\x.pdf`:                        "Other files (documents, projects, backups)",
		`C:\Users\Sam\Downloads\x (1).exe`:                    "Downloads",
		`C:\Users\Sam\AppData\Local\Temp\a\b.tmp`:             "Temp folders",
		`C:\Temp\x.bin`:                                       "Temp folders",
		`C:\tmp\x.bin`:                                        "Temp folders",
		`C:\src\app\node_modules\pkg\index.js`:                "Dev & build output",
		`C:\proj\.venv\Lib\site-packages\x.py`:                "Dev & build output",
		`C:\Users\Sam\AppData\Local\insomnia\app.asar`:        "App data & installed apps",
		`C:\ProgramData\Vendor\x.dll`:                         "App data & installed apps",
		`C:\Program Files\App\x.dll`:                          "App data & installed apps",
		`C:\hard\backup\photo.jpg`:                            "Other files (documents, projects, backups)",
		`C:\Users\Sam\AppData\Local\Temp\x\node_modules\y.js`: "Temp folders", // first match wins, top to bottom
		`C:\Users\Sam\Downloads\proj\node_modules\y.js`:       "Downloads",
		`c:/users/alice/downloads/x.exe`:                      "Other user accounts", // slashes and case don't matter
	}
	for path, want := range cases {
		i := cls.Category(path)
		if i < 0 || cls.Cats[i].Name != want {
			got := "(none)"
			if i >= 0 {
				got = cls.Cats[i].Name
			}
			t.Errorf("%s\n   got  %s\n   want %s", path, got, want)
		}
	}
}

func TestCustomCategoryRules(t *testing.T) {
	cats := []Category{
		{Name: "Videos", Rules: []string{".mp4$", "*.mkv"}},
		{Name: "Mine", Rules: []string{`^c:\hard\`, "{home}\\docs\\"}},
		{Name: "Regex", Rules: []string{`re:\\(old|bak)\d+\\`}},
		{Name: "Rest"},
	}
	if err := validateCategories(cats); err != nil {
		t.Fatal(err)
	}
	home := strings.ToLower(os.Getenv("USERPROFILE"))
	cls, err := newClassifier(cats, "sam")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		`D:\clips\a.MP4`:       "Videos",
		`D:\clips\a.mp4.txt`:   "Rest",
		`D:\clips\movie.mkv`:   "Videos",
		`C:\hard\x\y.bin`:      "Mine",
		`D:\C:\hard\y.bin`:     "Rest", // ^ anchors to the start of the path
		home + `\docs\a.txt`:   "Mine",
		`D:\data\old12\a.bin`:  "Regex",
		`D:\data\old\a.bin`:    "Rest",
		`D:\anything\else.bin`: "Rest",
	}
	for path, want := range cases {
		if got := cls.Cats[cls.Category(path)].Name; got != want {
			t.Errorf("%s: got %s, want %s", path, got, want)
		}
	}
	// no catch-all: unmatched paths report -1 rather than being forced somewhere
	only, _ := newClassifier([]Category{{Name: "X", Rules: []string{"zzz"}}}, "sam")
	if only.Category(`C:\a\b`) != -1 {
		t.Error("an unmatched path should be -1")
	}
}

func TestCategoryValidation(t *testing.T) {
	if err := validateCategories(nil); err == nil {
		t.Error("an empty list must be rejected")
	}
	if err := validateCategories([]Category{{Name: "  "}}); err == nil {
		t.Error("a blank name must be rejected")
	}
	if err := validateCategories([]Category{{Name: "Bad", Rules: []string{"re:("}}}); err == nil {
		t.Error("an invalid regular expression must be rejected")
	}
	cats := []Category{{Name: "A", Risk: "weird", Rules: []string{" x ", "", "  "}}, {Name: "A"}}
	if err := validateCategories(cats); err != nil {
		t.Fatal(err)
	}
	if cats[0].Risk != "review" || len(cats[0].Rules) != 1 || cats[0].Rules[0] != "x" {
		t.Errorf("not normalised: %+v", cats[0])
	}
	if cats[0].ID == cats[1].ID || cats[0].ID == "" {
		t.Errorf("ids must be unique and non-empty: %q %q", cats[0].ID, cats[1].ID)
	}
	path := filepath.Join(t.TempDir(), "categories.json")
	if err := saveCategories(path, cats); err != nil {
		t.Fatal(err)
	}
	if back := loadCategories(path); len(back) != 2 || back[0].Name != "A" {
		t.Errorf("round trip failed: %+v", back)
	}
	os.WriteFile(path, []byte("{not json"), 0o644)
	if back := loadCategories(path); len(back) != len(defaultCategories()) {
		t.Error("a broken file should fall back to the defaults")
	}
}

func TestPathHelpers(t *testing.T) {
	if location(`C:\hard\a\b.bin`) != `C:\hard` || location(`C:\Users\Sam\x\y.bin`) != `C:\Users\Sam` || location(`C:\f.bin`) != `C:` {
		t.Error("location")
	}
	if !hasPathPrefix(`C:\Users\Sam\x`, `c:\users\sam`) || hasPathPrefix(`C:\Users\Samuel`, `C:\Users\Sam`) || !hasPathPrefix(`C:\x`, `C:\`) {
		t.Error("hasPathPrefix")
	}
	if got := normalizeRoots([]string{`C:\a`, `C:\a\b`, `c:\A`, `D:`, ``}); len(got) != 2 || got[0] != `C:\a` || got[1] != `D:\` {
		t.Errorf("normalizeRoots: %v", got)
	}
	rules := parseExcludes([]string{"node_modules", `C:\Windows`, "*.tmp", "a?c"})
	for path, want := range map[string]bool{
		`C:\x\node_modules`: true, `C:\Windows\System32`: true, `C:\WindowsApps`: false,
		`C:\x\y.tmp`: true, `C:\x\abc`: true, `C:\x\abbc`: false,
	} {
		if got := rules.match(filepath.Base(path), path); got != want {
			t.Errorf("exclude %s: got %v want %v", path, got, want)
		}
	}
	if fmtBytes(1536) != "1.5 KB" || fmtBytes(5) != "5 B" {
		t.Error("fmtBytes")
	}
}
