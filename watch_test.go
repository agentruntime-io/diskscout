package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The folder that is open follows the disk: a file that appears in it shows up on the map without a rescan.
func TestLiveFollowsTheOpenFolder(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	open := filepath.Join(root, "Open")
	write(t, filepath.Join(open, "a.bin"), 700*1024, 1)
	write(t, filepath.Join(open, "b.bin"), 700*1024, 2)
	write(t, filepath.Join(root, "Other", "x.bin"), 700*1024, 3)
	write(t, filepath.Join(root, "Other", "y.bin"), 700*1024, 4)
	scanApp(t, a, root)
	defer a.SetLive(false, "")
	before := treeTotal(a, open)
	otherBefore := treeTotal(a, filepath.Join(root, "Other"))

	if err := a.SetLive(true, open); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // the watch is armed
	write(t, filepath.Join(open, "sub", "new.bin"), 2*1024*1024, 9)
	waitFor(t, "the new file to appear on the map", 20*time.Second, func() bool { return treeTotal(a, open) >= before+2*1024*1024 })

	// a change outside the open folder is not followed
	write(t, filepath.Join(root, "Other", "z.bin"), 2*1024*1024, 8)
	time.Sleep(4 * time.Second)
	if got := treeTotal(a, filepath.Join(root, "Other")); got != otherBefore {
		t.Fatalf("only the open folder is followed: Other changed from %d to %d", otherBefore, got)
	}

	// and a deletion shows too
	if err := os.Remove(filepath.Join(open, "a.bin")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deleted file to leave the map", 20*time.Second, func() bool { return treeTotal(a, open) < before+2*1024*1024-600*1024 })

	// turning it off ends the watch and lets go of the folder (a watched folder cannot be removed on Windows otherwise)
	if err := a.SetLive(false, ""); err != nil {
		t.Fatal(err)
	}
	if a.live.cancel != nil {
		t.Fatal("the watch should be gone")
	}
	if err := os.RemoveAll(filepath.Join(open, "sub")); err != nil {
		t.Fatalf("the folder was not let go: %v", err)
	}
}

func TestLiveRefusesWhatCannotBeFollowed(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "Open", "a.bin"), 700*1024, 1)
	write(t, filepath.Join(root, "Open", "b.bin"), 700*1024, 2)
	if err := a.SetLive(true, root); err == nil || !strings.Contains(err.Error(), "no space map") {
		t.Fatalf("with no scan: %v", err)
	}
	scanApp(t, a, root)
	defer a.SetLive(false, "")
	if err := a.SetLive(true, filepath.VolumeName(root)+`\`); err == nil || !strings.Contains(err.Error(), "whole drive") {
		t.Fatalf("a whole drive must be refused: %v", err)
	}
	if err := a.SetLive(true, filepath.Join(root, "Nowhere")); err == nil || !strings.Contains(err.Error(), "not on the map") {
		t.Fatalf("a folder that is not on the map must be refused: %v", err)
	}
	if err := a.SetLive(true, ""); err != nil || a.live.cancel != nil {
		t.Fatalf("an empty path just turns it off: %v", err)
	}
	// following a new folder lets go of the old one
	if err := a.SetLive(true, filepath.Join(root, "Open")); err != nil {
		t.Fatal(err)
	}
	first := a.live.done
	if err := a.SetLive(true, root); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	default:
		t.Fatal("the first watch should have ended before the second began")
	}
}

func TestLiveTargetsAreTheNearestMappedFolders(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	for _, d := range []string{"Open/One", "Open/Two", "Open/Three"} {
		write(t, filepath.Join(root, filepath.FromSlash(d), "a.bin"), 700*1024, 1)
		write(t, filepath.Join(root, filepath.FromSlash(d), "b.bin"), 700*1024, 2)
	}
	scanApp(t, a, root)
	open := filepath.Join(root, "Open")
	one, two := filepath.Join(open, "One"), filepath.Join(open, "Two")
	ch := func(ds ...string) map[string]string {
		m := map[string]string{}
		for _, d := range ds {
			m[normKey(d)] = d
		}
		return m
	}
	// a change deep in a folder too small to be on the map is read as its nearest mapped parent
	got := a.liveTargets(open, ch(filepath.Join(one, "tiny", "deeper")), false)
	if len(got) != 1 || !strings.EqualFold(got[0], one) {
		t.Fatalf("want One, got %v", got)
	}
	// two places, neither inside the other
	if got = a.liveTargets(open, ch(one, two), false); len(got) != 2 {
		t.Fatalf("want One and Two, got %v", got)
	}
	// a folder and something inside it: just the outer one
	if got = a.liveTargets(open, ch(open, one), false); len(got) != 1 || !strings.EqualFold(got[0], open) {
		t.Fatalf("want only Open, got %v", got)
	}
	// Windows lost the details: read the whole open folder
	if got = a.liveTargets(open, ch(one), true); len(got) != 1 || !strings.EqualFold(got[0], open) {
		t.Fatalf("an overflow means everything, got %v", got)
	}
	// the app's own files are not changes
	a.dataDir = filepath.Join(root, "data")
	if !a.liveIgnore(filepath.Join(root, "data", "deleted.log")) || a.liveIgnore(filepath.Join(open, "a.bin")) {
		t.Fatal("the app's own writes must be ignored, and only those")
	}
}

func TestParseNotify(t *testing.T) {
	if got := parseNotify(nil); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// two records: NextEntryOffset, Action, FileNameLength (bytes), then the UTF-16 name
	rec := func(next uint32, name string) []byte {
		u := []uint16{}
		for _, r := range name {
			u = append(u, uint16(r))
		}
		b := make([]byte, 12+len(u)*2)
		b[0], b[4], b[8] = byte(next), 1, byte(len(u)*2)
		b[1], b[2], b[3] = byte(next>>8), byte(next>>16), byte(next>>24)
		for i, c := range u {
			b[12+i*2], b[13+i*2] = byte(c), byte(c>>8)
		}
		return b
	}
	first := rec(0, `sub\a.txt`)
	first[0] = byte(len(first)) // the next record starts right after this one
	buf := append(first, rec(0, "b.bin")...)
	if got := parseNotify(buf); len(got) != 2 || got[0] != `sub\a.txt` || got[1] != "b.bin" {
		t.Fatalf("%v", got)
	}
}

func TestViewReportsHowMuchOfEachKindIsHere(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "Media", "clip.mp4"), 900*1024, 1)
	write(t, filepath.Join(root, "Media", "song.mp3"), 300*1024, 2)
	write(t, filepath.Join(root, "Media", "deep", "film.mkv"), 800*1024, 3)
	write(t, filepath.Join(root, "Media", "readme.txt"), 5*1024, 4) // small files count too
	scanApp(t, a, root)
	it, _, ok := a.tree.View(filepath.Join(root, "Media"), ViewOpts{Depth: 2, Limit: 50})
	if !ok {
		t.Fatal("no view")
	}
	if it.Kinds["video"] != 1700*1024 || it.Kinds["audio"] != 300*1024 || it.Kinds["doc"] != 5*1024 || it.Kinds["image"] != 0 {
		t.Fatalf("kinds: %v", it.Kinds)
	}
	for _, c := range it.Children {
		if c.Kinds != nil {
			t.Fatalf("only the folder being viewed carries the totals, not %s", c.Name)
		}
	}
}
