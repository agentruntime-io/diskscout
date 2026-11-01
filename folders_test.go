package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Folder A holds three big files and B holds four, and two of them are the same in both: the totals the folder view needs.
func TestDirFilesCountsWhatTheSearchSaw(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	A, B := filepath.Join(root, "A"), filepath.Join(root, "B")
	write(t, filepath.Join(A, "one.bin"), 300*1024, 1)
	write(t, filepath.Join(A, "two.bin"), 300*1024, 2)
	write(t, filepath.Join(A, "only-in-a.bin"), 300*1024, 3)
	write(t, filepath.Join(B, "uno.bin"), 300*1024, 1) // same bytes as A\one.bin, another name
	write(t, filepath.Join(B, "two.bin"), 300*1024, 2)
	write(t, filepath.Join(B, "b3.bin"), 300*1024, 4)
	write(t, filepath.Join(B, "b4.bin"), 300*1024, 5)
	scanApp(t, a, root)

	got := a.DirFiles([]string{A, B, filepath.Join(root, "nowhere")}, true)
	if ia := got[A]; !ia.Known || ia.Files != 3 || ia.Bytes != 3*300*1024 || len(ia.List) != 3 {
		t.Fatalf("A: %+v", ia)
	}
	if ib := got[B]; ib.Files != 4 || len(ib.List) != 4 {
		t.Fatalf("B: %+v", ib)
	}
	if n := got[filepath.Join(root, "nowhere")]; !n.Known || n.Files != 0 {
		t.Fatalf("a folder the search never saw has nothing, but the scan is known: %+v", n)
	}
	if plain := a.DirFiles([]string{A}, false)[A]; len(plain.List) != 0 || plain.Files != 3 {
		t.Fatalf("without list there are totals only: %+v", plain)
	}

	// what has gone since the scan is not counted
	if err := os.Remove(filepath.Join(A, "only-in-a.bin")); err != nil {
		t.Fatal(err)
	}
	if ia := a.DirFiles([]string{A}, false)[A]; ia.Files != 2 {
		t.Fatalf("a deleted file must not be counted: %+v", ia)
	}
	// a case-insensitive match, like the rest of the app on Windows
	if ib := a.DirFiles([]string{filepath.Join(root, "b")}, false)[filepath.Join(root, "b")]; ib.Files != 4 {
		t.Fatalf("folder names are compared without regard to case: %+v", ib)
	}
}

func TestDirFilesWithoutAScanIsUnknown(t *testing.T) {
	a := newTestApp(t)
	if got := a.DirFiles([]string{`C:\x`}, false)[`C:\x`]; got.Known {
		t.Fatalf("with no scan nothing is known: %+v", got)
	}
}
