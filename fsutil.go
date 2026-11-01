package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func fmtBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func fmtTime(ns int64) string { return time.Unix(0, ns).Format("2006-01-02 15:04") }

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	if strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + `\` + name
}

// normKey is the identity of a path for lookups: Windows paths are case-insensitive.
func normKey(p string) string { return strings.ToLower(filepath.Clean(p)) }

// hasPathPrefix reports whether path is prefix itself or lies below it, compared on whole segments.
func hasPathPrefix(path, prefix string) bool {
	p, q := strings.ToLower(path), strings.ToLower(strings.TrimRight(prefix, `\`))
	if !strings.HasPrefix(p, q) {
		return false
	}
	return len(p) == len(q) || p[len(q)] == '\\'
}

// location is the drive plus first folder, or drive + Users + account: `C:\hard`, `C:\Users\Sam`.
func location(path string) string {
	parts := strings.Split(strings.ReplaceAll(path, "/", `\`), `\`)
	depth := 2
	if len(parts) > 3 && strings.EqualFold(parts[1], "users") {
		depth = 3
	}
	n := depth
	if n > len(parts)-1 {
		n = len(parts) - 1
	}
	if n < 1 {
		n = 1
	}
	return strings.Join(parts[:n], `\`)
}

// normalizeRoots cleans the roots, drops duplicates and drops any root that lies inside another one.
func normalizeRoots(roots []string) []string {
	var clean []string
	for _, r := range roots {
		r = strings.TrimSpace(strings.Trim(r, `"`))
		if r == "" {
			continue
		}
		if len(r) == 2 && r[1] == ':' { // "D:" alone means "the current folder on D:"; filepath.Clean would turn it into "D:."
			r += `\`
		}
		clean = append(clean, filepath.Clean(r))
	}
	var out []string
next:
	for i, r := range clean {
		for j, o := range clean {
			if i == j {
				continue
			}
			if hasPathPrefix(r, o) && (!strings.EqualFold(r, o) || j < i) {
				continue next
			}
		}
		out = append(out, r)
	}
	return out
}

// excludeRules match names or paths. `name` matches a file or folder of that name anywhere; `C:\some\folder`
// matches that folder and everything below it; anything with * or ? is a glob over the name or the full path
// (and * also matches across folders).
type excludeRules []excludeRule

type excludeRule struct {
	pat  string
	wild bool
	sep  bool
}

func parseExcludes(pats []string) excludeRules {
	var out excludeRules
	for _, p := range pats {
		p = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(p, "/", `\`)))
		p = strings.TrimRight(p, `\`)
		if p == "" {
			continue
		}
		out = append(out, excludeRule{pat: p, wild: strings.ContainsAny(p, "*?"), sep: strings.Contains(p, `\`)})
	}
	return out
}

// match takes the entry's name and full path. Lowercasing is done here, once per call.
func (r excludeRules) match(name, full string) bool {
	if len(r) == 0 {
		return false
	}
	ln := strings.ToLower(name)
	var lf string
	for _, e := range r {
		switch {
		case e.wild:
			if lf == "" {
				lf = strings.ToLower(full)
			}
			if wildMatch(e.pat, ln) || wildMatch(e.pat, lf) {
				return true
			}
		case e.sep:
			if lf == "" {
				lf = strings.ToLower(full)
			}
			if lf == e.pat || strings.HasPrefix(lf, e.pat+`\`) {
				return true
			}
		default:
			if ln == e.pat {
				return true
			}
		}
	}
	return false
}

// wildMatch is a glob: * matches any run of characters (separators included), ? matches one.
func wildMatch(pat, s string) bool {
	p, i, star, mark := 0, 0, -1, 0
	for i < len(s) {
		switch {
		case p < len(pat) && (pat[p] == '?' || pat[p] == s[i]):
			p++
			i++
		case p < len(pat) && pat[p] == '*':
			star, mark = p, i
			p++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}
