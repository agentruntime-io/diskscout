package main

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const part = 700 * 1024 // two of these make a folder the map keeps (1 MB and up)

func blob(seed byte) []byte {
	b := make([]byte, part)
	for i := range b {
		b[i] = byte(i*31) + seed
	}
	return b
}

// makeZip writes a zip whose files are the given name -> seed pairs. store skips compression, so a flipped byte in the file
// shows up as a failed CRC-32 rather than a decompression error.
func makeZip(t *testing.T, zipPath string, files map[string]byte, store bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, seed := range files {
		method := zip.Deflate
		if store {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(blob(seed))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// unpack writes the same files under base, as an unpacked folder would hold them.
func unpack(t *testing.T, base string, files map[string]byte) {
	t.Helper()
	for name, seed := range files {
		p := filepath.Join(base, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, blob(seed), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func archiveApp(t *testing.T, root string) *App {
	t.Helper()
	a := newTestApp(t)
	scanApp(t, a, root)
	return a
}

func onlyArchive(t *testing.T, a *App) ArchiveInfo {
	t.Helper()
	list, known := a.Archives()
	if !known || len(list) != 1 {
		t.Fatalf("want one archive, got known=%v %+v", known, list)
	}
	return list[0]
}

func check(t *testing.T, a *App, in ArchiveInfo) *ArchiveCheck {
	t.Helper()
	c, err := a.CheckArchive(context.Background(), in.Zip, in.Base, in.Strip, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var packFiles = map[string]byte{"a.bin": 1, "sub/b.bin": 2}

func TestZipUnpackedIntoAFolderNamedAfterIt(t *testing.T) {
	root := t.TempDir()
	makeZip(t, filepath.Join(root, "Pack.zip"), packFiles, false)
	unpack(t, filepath.Join(root, "Pack"), packFiles)
	a := archiveApp(t, root)

	in := onlyArchive(t, a)
	if in.Kind != "zip" || !in.Readable || in.Entries != 2 || in.Same != 2 || in.Extras != 0 ||
		!strings.EqualFold(in.Base, filepath.Join(root, "Pack")) || !strings.EqualFold(in.Root, in.Base) {
		t.Fatalf("%+v", in)
	}
	c := check(t, a, in)
	if !c.ZipInFolder || !c.FolderInZip || c.Identical != 2 || c.BytesRead != 2*part || !c.ExtrasKnown ||
		!strings.HasPrefix(c.Verdict, "Identical") {
		t.Fatalf("%+v\n%s", c, c.Verdict)
	}
}

func TestFolderHoldsMoreThanTheZip(t *testing.T) {
	root := t.TempDir()
	makeZip(t, filepath.Join(root, "Pack.zip"), packFiles, false)
	unpack(t, filepath.Join(root, "Pack"), packFiles)
	os.WriteFile(filepath.Join(root, "Pack", "mine.txt"), []byte("added later"), 0o644)
	a := archiveApp(t, root)

	in := onlyArchive(t, a)
	if in.Extras != 1 {
		t.Fatalf("the folder has one file the zip lacks: %+v", in)
	}
	c := check(t, a, in)
	if !c.ZipInFolder || c.FolderInZip || c.Extras.N != 1 || !strings.Contains(c.Verdict, "also holds 1 file that is not in the zip") {
		t.Fatalf("%+v\n%s", c, c.Verdict)
	}
	// so the zip may go, and the folder may not
	var removed []string
	defer func(f func(string) error) { trashFile = f }(trashFile)
	trashFile = func(p string) error { removed = append(removed, p); return nil }
	res, _, err := a.TrashArchivePart(context.Background(), in.Zip, in.Base, in.Strip, "folder", nil)
	if err != nil || res.OK || !strings.Contains(res.Error, "not everything in the folder") || len(removed) != 0 {
		t.Fatalf("the folder must stay: %+v %v %v", res, err, removed)
	}
	res, _, err = a.TrashArchivePart(context.Background(), in.Zip, in.Base, in.Strip, "zip", nil)
	if err != nil || !res.OK || len(removed) != 1 || !strings.EqualFold(removed[0], in.Zip) {
		t.Fatalf("the zip may go: %+v %v %v", res, err, removed)
	}
}

func TestADamagedMissingOrChangedFileIsReported(t *testing.T) {
	root := t.TempDir()
	makeZip(t, filepath.Join(root, "Pack.zip"), map[string]byte{"a.bin": 1, "b.bin": 2, "c.bin": 3}, false)
	unpack(t, filepath.Join(root, "Pack"), map[string]byte{"a.bin": 1, "b.bin": 2, "c.bin": 3})
	a := archiveApp(t, root)
	in := onlyArchive(t, a)

	// a.bin: same size, one byte changed. b.bin: gone. c.bin: untouched.
	pa := filepath.Join(root, "Pack", "a.bin")
	b, _ := os.ReadFile(pa)
	b[len(b)/2] ^= 0xFF
	os.WriteFile(pa, b, 0o644)
	os.Remove(filepath.Join(root, "Pack", "b.bin"))

	c := check(t, a, in)
	if c.ZipInFolder || c.Identical != 1 || c.Different.N != 1 || c.Different.Names[0] != "a.bin" || c.Missing.N != 1 || c.Missing.Names[0] != "b.bin" {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(c.Verdict, "1 missing, 1 different") {
		t.Fatal(c.Verdict)
	}
	// neither part may be removed: the zip has what the folder lost, the folder holds a file that no longer matches
	var removed []string
	defer func(f func(string) error) { trashFile = f }(trashFile)
	trashFile = func(p string) error { removed = append(removed, p); return nil }
	for _, which := range []string{"zip", "folder"} {
		if res, _, err := a.TrashArchivePart(context.Background(), in.Zip, in.Base, in.Strip, which, nil); err != nil || res.OK {
			t.Fatalf("%s: %+v %v", which, res, err)
		}
	}
	if len(removed) != 0 {
		t.Fatalf("nothing may be removed: %v", removed)
	}
}

func TestADamagedZipEntryIsNotCalledIdentical(t *testing.T) {
	root := t.TempDir()
	zp := filepath.Join(root, "Pack.zip")
	makeZip(t, zp, map[string]byte{"a.bin": 1, "b.bin": 2}, true) // stored, so the bytes sit in the file as they are
	unpack(t, filepath.Join(root, "Pack"), map[string]byte{"a.bin": 1, "b.bin": 2})
	a := archiveApp(t, root)
	in := onlyArchive(t, a)

	raw, _ := os.ReadFile(zp)
	raw[200] ^= 0xFF // inside the first stored file
	os.WriteFile(zp, raw, 0o644)

	c := check(t, a, in)
	if c.ZipInFolder || c.Unreadable.N+c.Different.N != 1 || c.Identical != 1 {
		t.Fatalf("a zip entry that fails its checksum must not count as identical: %+v", c)
	}
}

func TestUnpackedBesideTheZipWithItsTopFolder(t *testing.T) {
	root := t.TempDir()
	files := map[string]byte{"Trip/a.bin": 1, "Trip/b.bin": 2}
	makeZip(t, filepath.Join(root, "download.zip"), files, false)
	unpack(t, root, files) // "extract here": ...\Trip\a.bin sits next to the zip
	os.WriteFile(filepath.Join(root, "unrelated.txt"), []byte("x"), 0o644)
	a := archiveApp(t, root)

	in := onlyArchive(t, a)
	if !strings.EqualFold(in.Base, root) || !strings.EqualFold(in.Root, filepath.Join(root, "Trip")) || in.Extras != 0 {
		t.Fatalf("the folder to judge is Trip, not the whole directory: %+v", in)
	}
	if c := check(t, a, in); !c.ZipInFolder || !c.FolderInZip {
		t.Fatalf("%+v", c)
	}
}

func TestUnpackedAmongOtherFilesCannotSayWhatElseIsThere(t *testing.T) {
	root := t.TempDir()
	files := map[string]byte{"a.bin": 1, "b.bin": 2}
	makeZip(t, filepath.Join(root, "flat.zip"), files, false)
	unpack(t, root, files) // straight into the folder the zip sits in
	a := archiveApp(t, root)

	in := onlyArchive(t, a)
	if in.Root != "" || in.Extras != -1 || !strings.Contains(in.Note, "among other files") {
		t.Fatalf("%+v", in)
	}
	c := check(t, a, in)
	if !c.ZipInFolder || c.FolderInZip || c.ExtrasKnown || !strings.Contains(c.Verdict, "cannot be told") {
		t.Fatalf("everything in the zip is there, but nothing can be said about the folder: %+v\n%s", c, c.Verdict)
	}
	if res, _, _ := a.TrashArchivePart(context.Background(), in.Zip, in.Base, in.Strip, "folder", nil); res.OK {
		t.Fatal("a folder shared with other files must never be offered for removal")
	}
}

func TestFolderRenamedOrMovedIsFoundByWhatIsInIt(t *testing.T) {
	root := t.TempDir()
	makeZip(t, filepath.Join(root, "Pack.zip"), packFiles, false)
	unpack(t, filepath.Join(root, "Elsewhere", "Renamed"), packFiles)
	a := archiveApp(t, root)

	in := onlyArchive(t, a)
	if !strings.EqualFold(in.Base, filepath.Join(root, "Elsewhere", "Renamed")) || in.Same != 2 {
		t.Fatalf("%+v", in)
	}
}

func TestOnlyAFewFilesUnpackedIsStillShownAsPartial(t *testing.T) {
	root := t.TempDir()
	files := map[string]byte{"a.bin": 1, "b.bin": 2, "c.bin": 3, "d.bin": 4}
	makeZip(t, filepath.Join(root, "Pack.zip"), files, false)
	unpack(t, filepath.Join(root, "Pack"), map[string]byte{"a.bin": 1, "b.bin": 2})
	a := archiveApp(t, root)
	in := onlyArchive(t, a)
	c := check(t, a, in)
	if in.Same != 2 || in.Entries != 4 || c.ZipInFolder || c.Missing.N != 2 || !c.FolderInZip {
		t.Fatalf("half unpacked: the zip is not covered, the folder is: %+v %+v", in, c)
	}
}

func TestRarAndSevenZipAreNamedButNotOpened(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "Game.rar"), make([]byte, 2048), 0o644)
	unpack(t, filepath.Join(root, "Game"), map[string]byte{"x.bin": 1})
	a := archiveApp(t, root)
	in := onlyArchive(t, a)
	if in.Kind != "RAR" || in.Readable || in.FolderFiles != 1 || !strings.Contains(in.Note, "only open .zip") {
		t.Fatalf("%+v", in)
	}
}

func TestUnrelatedZipAndFolderAreNotPaired(t *testing.T) {
	root := t.TempDir()
	makeZip(t, filepath.Join(root, "One.zip"), map[string]byte{"a.bin": 1}, false)
	unpack(t, filepath.Join(root, "Other"), map[string]byte{"z.bin": 9})
	a := archiveApp(t, root)
	if list, known := a.Archives(); !known || len(list) != 0 {
		t.Fatalf("nothing was unpacked: %+v", list)
	}
}

func TestArchiveNeedsAScanInThisWindow(t *testing.T) {
	a := newTestApp(t)
	if _, known := a.Archives(); known {
		t.Fatal("without a scan there is nothing to look through")
	}
}

func TestCompareZipToolAnswersFromTheContents(t *testing.T) {
	root := t.TempDir()
	makeZip(t, filepath.Join(root, "Pack.zip"), packFiles, false)
	unpack(t, filepath.Join(root, "Pack"), packFiles)
	a := archiveApp(t, root)
	out, err := toolCompareZip(context.Background(), a, map[string]any{"path": filepath.Join(root, "Pack.zip")})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["everything_in_zip_is_in_folder"] != true || m["everything_in_folder_is_in_zip"] != true || m["files_in_zip"] != 2 {
		t.Fatalf("%+v", m)
	}
	if _, err := toolCompareZip(context.Background(), a, map[string]any{"path": filepath.Join(t.TempDir(), "x.zip")}); err == nil {
		t.Fatal("a zip outside the scanned folders must be refused")
	}
}

func TestComparisonsAreRememberedWhileTheZipIsUnchanged(t *testing.T) {
	root := t.TempDir()
	zp := filepath.Join(root, "Pack.zip")
	makeZip(t, zp, packFiles, false)
	unpack(t, filepath.Join(root, "Pack"), packFiles)
	a := archiveApp(t, root)

	in := onlyArchive(t, a)
	if in.Check != nil || in.State() != "unchecked" {
		t.Fatalf("nothing has been compared yet: %+v", in)
	}
	check(t, a, in)
	in = onlyArchive(t, a)
	if in.Check == nil || !in.Check.ZipInFolder || in.State() != "safe" || in.Check.At == 0 || !strings.HasPrefix(in.Check.Verdict, "Identical") {
		t.Fatalf("the comparison should be remembered: %+v", in.Check)
	}
	// the zip is now a different file: what was said about it no longer holds
	later := time.Now().Add(time.Hour)
	os.Chtimes(zp, later, later)
	if in = onlyArchive(t, a); in.Check != nil || in.State() != "unchecked" {
		t.Fatalf("a changed zip must be compared again: %+v", in.Check)
	}
}

func TestStatesOfAZip(t *testing.T) {
	if (ArchiveInfo{Readable: true, Base: "x", Check: &ArchiveVerdict{ZipInFolder: true}}).State() != "safe" ||
		(ArchiveInfo{Readable: true, Base: "x", Check: &ArchiveVerdict{}}).State() != "different" ||
		(ArchiveInfo{Readable: true, Base: "x"}).State() != "unchecked" ||
		(ArchiveInfo{Readable: false, Base: "x", Kind: "RAR"}).State() != "cannot" ||
		(ArchiveInfo{Err: "bad zip"}).State() != "cannot" {
		t.Fatal("states")
	}
}

func TestCompareAllComparesEachZipAndRemembersIt(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"One", "Two", "Three"} {
		files := map[string]byte{n + "-a.bin": 1, n + "-b.bin": 2}
		makeZip(t, filepath.Join(root, n+".zip"), files, false)
		unpack(t, filepath.Join(root, n), files)
	}
	os.WriteFile(filepath.Join(root, "Two", "Two-a.bin"), make([]byte, part), 0o644) // Two's folder no longer matches its zip
	a := archiveApp(t, root)
	list, _ := a.Archives()
	if len(list) != 3 {
		t.Fatalf("%+v", list)
	}
	var items []ArchiveItem
	for _, i := range list {
		items = append(items, ArchiveItem{i.Zip, i.Base, i.Strip})
	}
	if err := a.CompareAll(items); err != nil {
		t.Fatal(err)
	}
	if err := a.CompareAll(items); err == nil {
		t.Fatal("a second comparison must not start while one runs")
	}
	waitFor(t, "every zip to be compared", 30*time.Second, func() bool {
		a.arcJob.mu.Lock()
		defer a.arcJob.mu.Unlock()
		return !a.arcJob.running
	})
	safe, different := 0, 0
	list, _ = a.Archives()
	for _, i := range list {
		switch i.State() {
		case "safe":
			safe++
		case "different":
			different++
		}
	}
	if safe != 2 || different != 1 {
		t.Fatalf("two zips are inside their folders and one is not: %d safe, %d different", safe, different)
	}
}

func TestAZipThatIsGoneIsNotListed(t *testing.T) {
	root := t.TempDir()
	zp := filepath.Join(root, "Pack.zip")
	makeZip(t, zp, packFiles, false)
	unpack(t, filepath.Join(root, "Pack"), packFiles)
	a := archiveApp(t, root)
	onlyArchive(t, a)
	os.Remove(zp) // moved to the Recycle Bin since the scan
	if list, _ := a.Archives(); len(list) != 0 {
		t.Fatalf("a zip that no longer exists must not be listed (let alone as unreadable): %+v", list)
	}
}

func TestZipsToolSummarisesTheList(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"One", "Two"} {
		files := map[string]byte{n + "-a.bin": 1, n + "-b.bin": 2}
		makeZip(t, filepath.Join(root, n+".zip"), files, false)
		unpack(t, filepath.Join(root, n), files)
	}
	os.WriteFile(filepath.Join(root, "Game.rar"), make([]byte, 2048), 0o644)
	unpack(t, filepath.Join(root, "Game"), map[string]byte{"x.bin": 1})
	a := archiveApp(t, root)
	list, _ := a.Archives()
	for _, i := range list {
		if strings.HasSuffix(i.Zip, "One.zip") {
			check(t, a, i) // only One has been compared
		}
	}
	out, err := toolZips(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["unpacked_zips"] != 3 || m["not_compared_yet"] != 1 || m["cannot_compare"] != 1 || m["not_the_same"] != 0 ||
		m["safe_to_remove"].(map[string]any)["count"] != 1 {
		t.Fatalf("%+v", m)
	}
	only, _ := toolZips(context.Background(), a, map[string]any{"only": "safe_to_remove"})
	rows := only.(map[string]any)["zips"].([]map[string]any)
	if len(rows) != 1 || !strings.HasSuffix(rows[0]["zip"].(string), "One.zip") || !strings.HasPrefix(rows[0]["compared"].(string), "Identical") {
		t.Fatalf("%+v", rows)
	}
	if _, err := toolZips(context.Background(), newTestApp(t), map[string]any{}); err == nil {
		t.Fatal("with no scan there is no list")
	}
}
