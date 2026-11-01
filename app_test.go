package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	return newApp(t.TempDir(), "tok")
}

func waitDone(t *testing.T, a *App) Status {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a.smu.Lock()
		running := a.running
		a.smu.Unlock()
		if !running {
			return a.Status()
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the scan did not finish")
	return Status{}
}

// fixture: one file with two older-or-newer copies, plus a unique file. Returns the root and the three copy paths.
func fixture(t *testing.T) (root string, copies []string) {
	root = t.TempDir()
	orig := filepath.Join(root, "keep", "orig.bin")
	c1 := filepath.Join(root, "dupe", "copy1.bin")
	c2 := filepath.Join(root, "dupe", "copy2.bin")
	for _, p := range []string{orig, c1, c2} {
		write(t, p, 400*1024, 1)
	}
	write(t, filepath.Join(root, "unique.bin"), 400*1024, 2)
	age(t, orig, 72*time.Hour)
	age(t, c1, 24*time.Hour)
	age(t, c2, 12*time.Hour)
	return root, []string{orig, c1, c2}
}

func scanApp(t *testing.T, a *App, root string) {
	t.Helper()
	cfg := Settings{Roots: []string{root}, FindDupes: true, MinDup: 1, Workers: 2}
	if err := a.StartScan(cfg); err != nil {
		t.Fatal(err)
	}
	if st := waitDone(t, a); st.Phase != "done" {
		t.Fatalf("scan ended as %q", st.Phase)
	}
}

func treeTotal(a *App, root string) int64 {
	n, _ := a.tree.Root.find(root)
	s, _, _ := n.sums()
	return s
}

func TestScanFeedsGroups(t *testing.T) {
	a := newTestApp(t)
	root, copies := fixture(t)
	scanApp(t, a, root)

	resp := a.GroupsSince(0, a.epoch)
	if len(resp.Groups) != 1 || len(resp.Groups[0].Copies) != 3 {
		t.Fatalf("want one group of 3, got %+v", resp)
	}
	g := resp.Groups[0]
	keeps := 0
	for _, c := range g.Copies {
		if c.Keep {
			keeps++
			if !strings.EqualFold(c.Path, copies[0]) {
				t.Errorf("the oldest copy should be the keeper, got %s", c.Path)
			}
		}
		if c.Cat < 0 || c.Loc == "" {
			t.Errorf("copy not classified: %+v", c)
		}
	}
	if keeps != 1 {
		t.Fatalf("%d keepers", keeps)
	}
	st := a.Status()
	if st.Groups != 1 || st.Reclaimable != 2*400*1024 || st.Files != 4 || st.Bytes != 4*400*1024 {
		t.Fatalf("status: %+v", st)
	}
	if again := a.GroupsSince(resp.Rev, resp.Epoch); len(again.Groups) != 0 {
		t.Fatalf("nothing changed, but %d groups came back", len(again.Groups))
	}
	if old := a.GroupsSince(resp.Rev, resp.Epoch-1); len(old.Groups) != 1 {
		t.Fatal("a caller on an old epoch must get everything")
	}
	if !a.isDup(copies[1]) || a.isDup(filepath.Join(root, "unique.bin")) {
		t.Fatal("isDup is wrong")
	}
	if err := a.StartScan(Settings{Roots: []string{root}}); err != nil {
		t.Fatalf("a second scan after the first finished should start: %v", err)
	}
	waitDone(t, a)
}

func TestScanRefusesWhileRunning(t *testing.T) {
	a := newTestApp(t)
	a.running = true
	if err := a.StartScan(Settings{Roots: []string{t.TempDir()}}); err != errBusy {
		t.Fatalf("got %v", err)
	}
	a.running = false
	if err := a.StartScan(Settings{}); err == nil {
		t.Fatal("a scan with no folders must be refused")
	}
}

func TestDeleteGuards(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	size := int64(400 * 1024)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	start := treeTotal(a, root)

	res, _ := a.Delete([]string{filepath.Join(root, "unique.bin")}, "permanent", false)
	if res[0].OK || res[0].Error != "not part of this report" || !exists(filepath.Join(root, "unique.bin")) {
		t.Fatalf("a file outside the report must be refused: %+v", res[0])
	}
	if _, err := a.Delete(c, "shred", false); err == nil {
		t.Fatal("an unknown mode must be refused")
	}

	res, _ = a.Delete(c, "permanent", false) // all three at once
	for _, r := range res {
		if r.OK || !strings.Contains(r.Error, "every remaining copy") {
			t.Fatalf("removing every copy must be refused: %+v", r)
		}
	}
	for _, p := range c {
		if !exists(p) {
			t.Fatalf("%s was deleted", p)
		}
	}

	res, _ = a.Delete([]string{c[1], c[2]}, "permanent", false)
	if !res[0].OK || !res[1].OK || res[0].Size != size || exists(c[1]) || exists(c[2]) || !exists(c[0]) {
		t.Fatalf("two copies should go and the keeper stay: %+v", res)
	}
	if got := treeTotal(a, root); got != start-2*size {
		t.Fatalf("the map should shrink by %d, now %d (was %d)", 2*size, got, start)
	}
	if n, _ := a.liveStats(); n != 0 {
		t.Fatalf("one copy left is not a duplicate group, but %d are live", n)
	}
	log, _ := os.ReadFile(filepath.Join(a.dataDir, "deleted.log"))
	if strings.Count(string(log), "\n") != 2 {
		t.Fatalf("deleted.log should hold 2 lines:\n%s", log)
	}

	res, _ = a.Delete([]string{c[0]}, "permanent", false)
	if res[0].OK || !exists(c[0]) {
		t.Fatalf("the last copy needs allowAll: %+v", res[0])
	}
	res, _ = a.Delete([]string{c[0]}, "permanent", true)
	if !res[0].OK || exists(c[0]) {
		t.Fatalf("allowAll should remove it: %+v", res[0])
	}
	res, _ = a.Delete([]string{c[0]}, "permanent", true)
	if res[0].OK || !res[0].Gone {
		t.Fatalf("a repeated delete should say it is gone: %+v", res[0])
	}
}

func TestDeleteRefusesChangedFile(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	write(t, c[1], 400*1024, 7) // same size, new content and modified time
	res, _ := a.Delete([]string{c[1]}, "permanent", false)
	if res[0].OK || res[0].Error != "changed since the scan" {
		t.Fatalf("%+v", res[0])
	}
	if _, err := os.Stat(c[1]); err != nil {
		t.Fatal("the changed file must still exist")
	}
}

// A copy that has gone bad since the scan, with the same size and modified time, must not be the one left behind.
func TestDeleteChecksTheCopyThatStays(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	// c[0] is the suggested KEEP. Damage it in place, restoring size and modified time so only its content differs.
	st, _ := os.Stat(c[0])
	b, _ := os.ReadFile(c[0])
	b[len(b)/2] ^= 0xFF
	if err := os.WriteFile(c[0], b, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(c[0], st.ModTime(), st.ModTime())

	res, _ := a.Delete([]string{c[1]}, "permanent", false)
	if res[0].OK || !strings.Contains(res[0].Error, "no longer matches") || !exists(c[1]) {
		t.Fatalf("a damaged keeper must stop the delete: %+v", res[0])
	}
	// the user can still choose speed over the check; the same guards otherwise apply
	res, _ = a.DeleteUnchecked([]string{c[1]}, "permanent", false)
	if !res[0].OK || res[0].Verified != "" || exists(c[1]) {
		t.Fatalf("an unchecked delete should go ahead: %+v", res[0])
	}
}

func TestDeleteRefusesWhenTheKeeperCannotBeRead(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	real := digestFile
	defer func() { digestFile = real }()
	digestFile = func(p string, buf []byte) (string, error) {
		if p == c[0] || p == c[2] { // every copy that would stay
			return "", errors.New("simulated read error")
		}
		return real(p, buf)
	}
	res, _ := a.Delete([]string{c[1]}, "permanent", false)
	if res[0].OK || !strings.Contains(res[0].Error, "could not be read") {
		t.Fatalf("%+v", res[0])
	}
	if _, err := os.Stat(c[1]); err != nil {
		t.Fatal("nothing may be removed when the copy that stays cannot be read")
	}
}

func TestDeleteRecordsWhatItWasProvenAgainst(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	res, _ := a.Delete([]string{c[1], c[2]}, "permanent", false)
	for _, r := range res {
		if !r.OK || !strings.EqualFold(r.Verified, c[0]) { // c[0] is the keeper and the one it is compared with
			t.Fatalf("each removed copy should name the keeper it matched: %+v", r)
		}
	}
	log, _ := os.ReadFile(filepath.Join(a.dataDir, "deleted.log"))
	if strings.Count(string(log), `"identical_to"`) != 2 {
		t.Fatalf("deleted.log should say what each copy was compared with:\n%s", log)
	}
}

func TestKeeperPromotedWhenDeleted(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	if res, _ := a.Delete([]string{c[0]}, "permanent", false); !res[0].OK { // c[0] is the keeper; two others survive
		t.Fatalf("%+v", res[0])
	}
	g := a.GroupsSince(0, a.epoch).Groups[0]
	live, keeps := 0, 0
	for _, cp := range g.Copies {
		if !cp.Gone {
			live++
			if cp.Keep {
				keeps++
			}
		}
	}
	if live != 2 || keeps != 1 {
		t.Fatalf("after deleting the keeper: %d live, %d keepers", live, keeps)
	}
}

func TestVerifyNoticesExternalDeletes(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	scanApp(t, a, root)
	before := a.GroupsSince(0, a.epoch).Rev
	treeBefore := treeTotal(a, root)
	os.Remove(c[2]) // deleted in Explorer, say
	if n := a.Verify(); n != 1 {
		t.Fatalf("verify found %d missing", n)
	}
	resp := a.GroupsSince(before, a.epoch)
	if len(resp.Groups) != 1 {
		t.Fatalf("the changed group should be re-sent, got %d", len(resp.Groups))
	}
	if gone := countGone(resp.Groups[0]); gone != 1 {
		t.Fatalf("%d copies marked gone", gone)
	}
	if treeTotal(a, root) != treeBefore-400*1024 {
		t.Fatal("the map should have lost the file too")
	}
	if a.Verify() != 0 {
		t.Fatal("a second verify finds nothing new")
	}
}

func countGone(g GroupJSON) int {
	n := 0
	for _, c := range g.Copies {
		if c.Gone {
			n++
		}
	}
	return n
}

func TestTrashPathGuards(t *testing.T) {
	a := newTestApp(t)
	if r := a.TrashPath(`C:\anything`); r.OK || !strings.Contains(r.Error, "no space map") {
		t.Fatalf("without a map: %+v", r)
	}
	root, c := fixture(t)
	scanApp(t, a, root)
	for path, why := range map[string]string{
		`C:\`:                   "whole drive",
		os.Getenv("SystemRoot"): "system or profile",
		filepath.Join(os.Getenv("SystemRoot"), "System32"): "inside Windows",
		os.Getenv("USERPROFILE"):                           "system or profile",
		filepath.Join(t.TempDir(), "x"):                    "outside the scanned",
	} {
		if r := a.TrashPath(path); r.OK || !strings.Contains(r.Error, why) {
			t.Errorf("%s: want %q, got %+v", path, why, r)
		}
	}
	if r := a.TrashPath(filepath.Join(root, "missing.bin")); r.OK || !r.Gone {
		t.Errorf("a missing file: %+v", r)
	}
	if os.Getenv("DISKSCOUT_TEST_RECYCLE") == "" {
		t.Skip("set DISKSCOUT_TEST_RECYCLE=1 to really send a file to the Recycle Bin")
	}
	before := treeTotal(a, root)
	if r := a.TrashPath(c[1]); !r.OK {
		t.Fatalf("recycling failed: %+v", r)
	}
	if _, err := os.Stat(c[1]); err == nil {
		t.Fatal("the file is still there")
	}
	if treeTotal(a, root) != before-400*1024 {
		t.Fatal("the map did not shrink")
	}
}

func TestImportReport(t *testing.T) {
	a := newTestApp(t)
	root, c := fixture(t)
	mt := func(p string) int64 { st, _ := os.Stat(p); return st.ModTime().UnixNano() }
	type cp = map[string]any
	doc := map[string]any{
		"generated": "2026-10-28T02:17:10", "roots": []string{root}, "summary": map[string]any{"files_scanned": 4, "bytes_scanned": 1600},
		"groups": []any{
			map[string]any{"id": 1, "size": 400 * 1024, "blake2b256": "abc", "copies": []cp{
				{"path": c[0], "mtime_ns": mt(c[0]), "keep": true, "hardlinks": []string{}},
				{"path": c[1], "mtime_ns": mt(c[1]), "keep": false, "hardlinks": []string{}},
				{"path": filepath.Join(root, "already-deleted.bin"), "mtime_ns": 1, "keep": false, "hardlinks": []string{}},
			}},
			map[string]any{"id": 2, "size": 5, "blake2b256": "x", "copies": []cp{{"path": "C:\\only-one.bin", "mtime_ns": 1, "keep": true, "hardlinks": []string{}}}},
		},
	}
	file := filepath.Join(t.TempDir(), "duplicates.json")
	b, _ := json.Marshal(doc)
	os.WriteFile(file, b, 0o644)

	n, err := a.Import(filepath.Dir(file)) // a folder works as well as the file
	if err != nil || n != 1 {
		t.Fatalf("import: %d groups, %v", n, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && a.GroupsSince(0, a.epoch).Groups[0].Copies[2].Gone == false {
		time.Sleep(20 * time.Millisecond) // the verify pass runs in the background
	}
	g := a.GroupsSince(0, a.epoch).Groups[0]
	if len(g.Copies) != 3 || !g.Copies[2].Gone || g.Copies[0].Gone {
		t.Fatalf("a copy that no longer exists should be marked gone: %+v", g.Copies)
	}
	if st := a.Status(); st.Source != "import" || st.Phase != "done" || st.Files != 4 {
		t.Fatalf("status: %+v", st)
	}
	// and what was imported can be acted on, with the same guards
	if res, _ := a.Delete([]string{c[1]}, "permanent", false); !res[0].OK {
		t.Fatalf("deleting an imported copy: %+v", res[0])
	}

	delete(doc["groups"].([]any)[0].(map[string]any)["copies"].([]cp)[0], "mtime_ns")
	b, _ = json.Marshal(doc)
	os.WriteFile(file, b, 0o644)
	if _, err := a.Import(file); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("an old report without modified times must be refused, got %v", err)
	}
	if _, err := a.Import(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestCategoryEditReclassifies(t *testing.T) {
	a := newTestApp(t)
	root, _ := fixture(t)
	scanApp(t, a, root)
	first := a.GroupsSince(0, a.epoch)
	_, rev0 := a.Categories()

	if err := a.SetCategories([]Category{{Name: "Bad", Rules: []string{"re:("}}}); err == nil {
		t.Fatal("an invalid rule must be refused")
	}
	if _, rev := a.Categories(); rev != rev0 {
		t.Fatal("a refused edit must not change anything")
	}
	cats := []Category{{Name: "Mine", Rules: []string{"\\keep\\"}}, {Name: "The rest"}}
	if err := a.SetCategories(cats); err != nil {
		t.Fatal(err)
	}
	if _, rev := a.Categories(); rev != rev0+1 {
		t.Fatal("the category revision should advance")
	}
	again := a.GroupsSince(first.Rev, a.epoch)
	if len(again.Groups) != 1 {
		t.Fatal("changed categories mean the groups must be sent again")
	}
	for _, c := range again.Groups[0].Copies {
		want := 1
		if strings.Contains(c.Path, `\keep\`) {
			want = 0
		}
		if c.Cat != want {
			t.Errorf("%s: category %d, want %d", c.Path, c.Cat, want)
		}
	}
	saved := loadCategories(filepath.Join(a.dataDir, "categories.json"))
	if len(saved) != 2 || saved[0].Name != "Mine" {
		t.Fatalf("not persisted: %+v", saved)
	}
	b := newApp(a.dataDir, "x") // a fresh start picks the edited definitions up
	if got, _ := b.Categories(); len(got) != 2 {
		t.Fatalf("restart lost the categories: %+v", got)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReports(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "duplicates.json"), 10, 1)
	write(t, filepath.Join(base, "report-a", "duplicates.json"), 20, 1)
	write(t, filepath.Join(base, "report-b", "duplicates.json"), 30, 1)
	write(t, filepath.Join(base, "report-b", "deeper", "duplicates.json"), 30, 1) // two levels down: not looked at
	write(t, filepath.Join(base, "other", "notes.txt"), 5, 1)
	age(t, filepath.Join(base, "report-a", "duplicates.json"), 48*time.Hour)
	age(t, filepath.Join(base, "report-b", "duplicates.json"), 24*time.Hour)
	got := discoverReports([]string{base, base, "", filepath.Join(base, "missing")}) // repeats and gaps are fine
	if len(got) != 3 {
		t.Fatalf("want 3 reports, got %+v", got)
	}
	if !strings.HasSuffix(got[0].Path, "duplicates.json") || strings.Contains(got[0].Path, "report-") || !strings.Contains(got[1].Path, "report-b") || !strings.Contains(got[2].Path, "report-a") {
		t.Fatalf("not newest first: %+v", got)
	}
}

func TestScanKeepsPrefsAndClaudeDoesNotRewriteSettings(t *testing.T) {
	a := newTestApp(t)
	a.settings.Prefs = map[string]any{"sys": false, "sidemode": "big"}
	root, _ := fixture(t)
	scanApp(t, a, root) // the dialog's settings carry no prefs and no schema
	if a.settings.Prefs["sys"] != false || a.settings.Prefs["sidemode"] != "big" || a.settings.Schema != 2 {
		t.Fatalf("a scan wiped the window preferences: %+v schema %d", a.settings.Prefs, a.settings.Schema)
	}
	saved, _ := os.ReadFile(filepath.Join(a.dataDir, "settings.json"))
	if !strings.Contains(string(saved), `"sidemode": "big"`) {
		t.Fatalf("preferences not saved with the scan: %s", saved)
	}

	other := t.TempDir()
	write(t, filepath.Join(other, "f.bin"), 100, 1)
	before := a.settings
	if err := a.startScan(Settings{Roots: []string{other}, FindDupes: true, MinDup: 1}, false); err != nil { // as Claude's scan tool does
		t.Fatal(err)
	}
	waitDone(t, a)
	if len(a.settings.Roots) != len(before.Roots) || a.settings.Roots[0] != before.Roots[0] || a.settings.FindDupes != before.FindDupes {
		t.Fatalf("a scan started by Claude changed the user's saved settings: %+v", a.settings)
	}
	if st := a.Status(); st.Files != 1 {
		t.Fatalf("but it did scan what it was asked to: %+v", st)
	}
}
