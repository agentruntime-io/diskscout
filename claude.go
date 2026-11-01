package main

// How Claude uses the app safely.
//
// Claude (the chat panel, Claude Code, or Claude Desktop, all through MCP) can look at everything and can move the
// window around. It can never delete on its own: a request to remove files waits in the window until the user presses
// Approve, and only ever moves things to the Recycle Bin. Suggestions go to the user's "to clean" list instead.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ApprovalItem struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Kind string `json:"kind"` // file | dir
}

type Approval struct {
	ID      string         `json:"id"`
	Actor   string         `json:"actor"`
	Kind    string         `json:"kind"` // duplicates | paths
	Reason  string         `json:"reason"`
	Items   []ApprovalItem `json:"items"`
	Total   int64          `json:"total"`
	Created int64          `json:"created"`
	Command string         `json:"command,omitempty"` // kind "command": the exact command line
	Class   string         `json:"class,omitempty"`   // ...and whether it is a cleanup or an unlisted command
	Note    string         `json:"note,omitempty"`
	res     chan approvalDecision
}

type approvalDecision struct{ approved, trust bool }

type Suggestion struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	By     string `json:"by"`
}

type ViewState struct {
	Tab      string `json:"tab"`
	Path     string `json:"path"`
	Selected string `json:"selected"`
	Filter   string `json:"filter"`
}

type claudeState struct {
	mu          sync.Mutex
	pending     map[string]*Approval
	suggestions []Suggestion
	view        ViewState
	trusted     bool // the user said "don't ask again" for Recycle Bin moves, until the window closes
}

var approvalTimeout = 10 * time.Minute

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) pendingApprovals() []Approval {
	a.cl.mu.Lock()
	defer a.cl.mu.Unlock()
	out := []Approval{}
	for _, p := range a.cl.pending {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// describePath says what a path is on the map: its size and whether it is a folder. ok is false if it should not be
// offered (protected, outside the scan, or gone).
func (a *App) describePath(path string) (it ApprovalItem, why string) {
	path = filepath.Clean(path)
	it = ApprovalItem{Path: path, Name: filepath.Base(path)}
	if w := protectedPath(path); w != "" {
		return it, w
	}
	a.mu.RLock()
	t := a.tree
	a.mu.RUnlock()
	if t == nil {
		return it, "there is no space map yet: run a scan first"
	}
	inside := false
	for _, r := range t.Roots {
		if hasPathPrefix(path, r) {
			inside = true
		}
	}
	if !inside {
		return it, "outside the scanned folders"
	}
	st, err := os.Lstat(path)
	if err != nil {
		return it, "does not exist"
	}
	if fileAttrs(st)&skipAttrs != 0 {
		return it, "that is a link or a cloud placeholder"
	}
	if st.IsDir() {
		it.Kind = "dir"
		n, exact := t.Root.find(path)
		if n == nil || !exact {
			return it, "that folder is not on the map (too small), so its size is unknown"
		}
		it.Size, _, _ = n.sums()
	} else {
		it.Kind, it.Size = "file", st.Size()
	}
	return it, ""
}

// RequestTrash asks the user, in the window, to move things to the Recycle Bin on Claude's behalf, and waits for the
// answer. kind is "duplicates" (paths from a duplicate group) or "paths" (items from the space map).
func (a *App) RequestTrash(ctx context.Context, actor, kind string, paths []string, reason string) ([]DelResult, error) {
	var results []DelResult
	var items []ApprovalItem
	var valid []string
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[normKey(p)] {
			continue
		}
		seen[normKey(p)] = true
		switch kind {
		case "duplicates":
			a.mu.RLock()
			r, ok := a.index[normKey(p)]
			live := ok && !r.c.Gone
			var size int64
			if ok {
				size = r.g.Size
			}
			a.mu.RUnlock()
			if !live {
				results = append(results, DelResult{Path: p, Error: "not a duplicate DiskScout has listed (or already gone)"})
				continue
			}
			items = append(items, ApprovalItem{Path: p, Name: filepath.Base(p), Size: size, Kind: "file"})
		case "paths":
			it, why := a.describePath(p)
			if why != "" {
				results = append(results, DelResult{Path: p, Error: why})
				continue
			}
			items = append(items, it)
		default:
			return nil, fmt.Errorf(`kind must be "duplicates" or "paths"`)
		}
		valid = append(valid, p)
	}
	if len(items) == 0 {
		return results, nil
	}
	if a.hub.count() == 0 {
		return nil, errors.New("no DiskScout window is open, so nobody can approve this. Ask the user to open the DiskScout window")
	}

	a.cl.mu.Lock()
	trusted := a.cl.trusted
	a.cl.mu.Unlock()
	if !trusted {
		ap := &Approval{Actor: actor, Kind: kind, Reason: reason, Items: items}
		for _, it := range items {
			ap.Total += it.Size
		}
		d, err := a.ask(ctx, ap)
		if err != nil {
			return nil, err
		}
		if !d.approved {
			return nil, errors.New("the user declined this request")
		}
		if d.trust {
			a.cl.mu.Lock()
			a.cl.trusted = true
			a.cl.mu.Unlock()
			a.hub.Broadcast("toast", map[string]string{"text": actor + " may now move files to the Recycle Bin without asking, until you close this window."})
		}
	}

	switch kind {
	case "duplicates":
		res, err := a.deleteAs(actor, valid, "trash", false, true) // Claude's deletes are always byte-checked
		if err != nil {
			return nil, err
		}
		results = append(results, res...)
	default:
		for _, p := range valid {
			results = append(results, a.trashPathAs(actor, p))
		}
	}
	return results, nil
}

// ask puts a request in front of the user in the window and waits for the answer.
func (a *App) ask(ctx context.Context, ap *Approval) (approvalDecision, error) {
	ap.ID, ap.Created, ap.res = newID(), time.Now().UnixMilli(), make(chan approvalDecision, 1)
	a.cl.mu.Lock()
	a.cl.pending[ap.ID] = ap
	a.cl.mu.Unlock()
	a.hub.Broadcast("approval", ap)
	defer func() {
		a.cl.mu.Lock()
		delete(a.cl.pending, ap.ID)
		a.cl.mu.Unlock()
		a.hub.Broadcast("approval-done", map[string]string{"id": ap.ID})
	}()
	select {
	case d := <-ap.res:
		return d, nil
	case <-ctx.Done():
		a.hub.Broadcast("toast", map[string]string{"text": ap.Actor + " stopped waiting, so that request was withdrawn. Nothing was done."})
		return approvalDecision{}, errors.New("the request was cancelled before the user answered")
	case <-time.After(approvalTimeout):
		return approvalDecision{}, errors.New("the user did not answer within 10 minutes, so nothing was done")
	}
}

// ResolveApproval records the user's answer to a pending request.
func (a *App) ResolveApproval(id string, approve, trust bool) bool {
	a.cl.mu.Lock()
	ap := a.cl.pending[id]
	a.cl.mu.Unlock()
	if ap == nil {
		return false
	}
	select {
	case ap.res <- approvalDecision{approve, trust}:
	default:
	}
	return true
}

// Suggest puts items on the user's "to clean" list, with Claude's reason, without removing anything.
func (a *App) Suggest(by string, items []struct{ Path, Reason string }) ([]Suggestion, []DelResult) {
	var ok []Suggestion
	var bad []DelResult
	for _, in := range items {
		it, why := a.describePath(in.Path)
		if why != "" {
			bad = append(bad, DelResult{Path: in.Path, Error: why})
			continue
		}
		ok = append(ok, Suggestion{Path: it.Path, Name: it.Name, Size: it.Size, Kind: it.Kind, Reason: strings.TrimSpace(in.Reason), By: by})
	}
	if len(ok) > 0 {
		a.cl.mu.Lock()
		have := map[string]bool{}
		for _, s := range a.cl.suggestions {
			have[normKey(s.Path)] = true
		}
		for _, s := range ok {
			if !have[normKey(s.Path)] {
				a.cl.suggestions = append(a.cl.suggestions, s)
			}
		}
		if len(a.cl.suggestions) > 300 {
			a.cl.suggestions = a.cl.suggestions[len(a.cl.suggestions)-300:]
		}
		a.cl.mu.Unlock()
		a.hub.Broadcast("suggest", ok)
	}
	return ok, bad
}

func (a *App) AckSuggestions() {
	a.cl.mu.Lock()
	a.cl.suggestions = nil
	a.cl.mu.Unlock()
}

func (a *App) pendingSuggestions() []Suggestion {
	a.cl.mu.Lock()
	defer a.cl.mu.Unlock()
	return append([]Suggestion{}, a.cl.suggestions...)
}

func (a *App) SetView(v ViewState) {
	a.cl.mu.Lock()
	a.cl.view = v
	a.cl.mu.Unlock()
}

func (a *App) View() ViewState {
	a.cl.mu.Lock()
	defer a.cl.mu.Unlock()
	return a.cl.view
}

// Show moves the user's window: switch tab, open a folder, select an item. This is how Claude "uses the app" in view.
func (a *App) Show(tab, path, sel string) {
	msg := map[string]string{}
	switch tab {
	case "space", "dups":
		msg["tab"] = tab
	case "zips": // the Zips list lives in the duplicates tab
		msg["tab"], msg["view"] = "dups", "zips"
	}
	if path != "" {
		msg["path"] = filepath.Clean(path)
	}
	if sel != "" {
		msg["select"] = filepath.Clean(sel)
	}
	a.hub.Broadcast("ui", msg)
}
