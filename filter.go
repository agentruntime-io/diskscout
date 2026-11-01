package main

// The filter box. Terms are separated by spaces and ALL must match; they apply to the files the map names (1 MB and up).
//
//	*.mov|*.mp4   a name pattern; | separates alternatives, * and ? are wildcards
//	whatsapp      plain text: the path contains it
//	-node_modules a leading minus means "must NOT match"
//	>500MB  <10KB size (B, KB, MB, GB, TB)
//	<30d  >1y     age since last modified: shorter than 30 days, older than a year (h, d, w, m = 30 days, y)
//	type:video    video, audio, image, archive, doc, code, exe, other
//	dup           only files that have an identical copy elsewhere

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Filter struct {
	terms []fterm
	now   int64
}

type fterm struct {
	neg  bool
	kind byte // s size, a age, t type, d duplicate, g text or pattern
	op   string
	num  float64
	typ  string
	alts []string
}

var cmpRe = regexp.MustCompile(`(?i)^(<=|>=|<|>)(\d+(?:\.\d+)?)(b|kb|mb|gb|tb|h|d|w|m|y)$`)

var unitBytes = map[string]float64{"b": 1, "kb": 1 << 10, "mb": 1 << 20, "gb": 1 << 30, "tb": 1 << 40}
var unitAge = map[string]float64{
	"h": float64(time.Hour), "d": 24 * float64(time.Hour), "w": 7 * 24 * float64(time.Hour),
	"m": 30 * 24 * float64(time.Hour), "y": 365 * 24 * float64(time.Hour),
}

// parseFilter returns nil for an empty expression. Terms that cannot be understood are returned in bad and ignored.
func parseFilter(expr string) (*Filter, []string) {
	var f Filter
	var bad []string
	f.now = time.Now().UnixNano()
	for _, tok := range strings.Fields(expr) {
		t := fterm{}
		if len(tok) > 1 && tok[0] == '-' {
			t.neg, tok = true, tok[1:]
		}
		low := strings.ToLower(tok)
		switch {
		case low == "dup" || low == "dups":
			t.kind = 'd'
		case strings.HasPrefix(low, "type:"):
			if _, ok := typeExts[low[5:]]; !ok && low[5:] != "other" {
				bad = append(bad, tok)
				continue
			}
			t.kind, t.typ = 't', low[5:]
		case low != "" && (low[0] == '<' || low[0] == '>'):
			m := cmpRe.FindStringSubmatch(low)
			if m == nil {
				bad = append(bad, tok)
				continue
			}
			n, _ := strconv.ParseFloat(m[2], 64)
			t.op = m[1]
			if mul, ok := unitBytes[m[3]]; ok {
				t.kind, t.num = 's', n*mul
			} else {
				t.kind, t.num = 'a', n*unitAge[m[3]]
			}
		default:
			t.kind = 'g'
			for _, a := range strings.Split(strings.ReplaceAll(low, "/", `\`), "|") {
				if a != "" {
					t.alts = append(t.alts, a)
				}
			}
			if len(t.alts) == 0 {
				continue
			}
		}
		f.terms = append(f.terms, t)
	}
	if len(f.terms) == 0 {
		return nil, bad
	}
	return &f, bad
}

func compare(v float64, op string, n float64) bool {
	switch op {
	case "<":
		return v < n
	case "<=":
		return v <= n
	case ">":
		return v > n
	}
	return v >= n
}

// Match reports whether a file passes every term.
func (f *Filter) Match(path, name string, size, mtime int64, dup bool) bool {
	if f == nil {
		return true
	}
	lp, ln := strings.ToLower(path), strings.ToLower(name)
	for _, t := range f.terms {
		ok := false
		switch t.kind {
		case 's':
			ok = compare(float64(size), t.op, t.num)
		case 'a':
			ok = compare(float64(f.now-mtime), t.op, t.num)
		case 't':
			ok = fileTypeOf(name) == t.typ
		case 'd':
			ok = dup
		default:
			for _, a := range t.alts {
				if strings.ContainsAny(a, "*?") {
					target := ln
					if strings.Contains(a, `\`) {
						target = lp
					}
					if wildMatch(a, target) {
						ok = true
					}
				} else if strings.Contains(lp, a) {
					ok = true
				}
			}
		}
		if ok == t.neg {
			return false
		}
	}
	return true
}

var typeExts = map[string][]string{
	"video":   {"mp4", "mov", "mkv", "avi", "webm", "wmv", "m4v", "flv", "mpg", "mpeg"},
	"audio":   {"mp3", "wav", "flac", "m4a", "aac", "ogg", "wma", "aiff"},
	"image":   {"jpg", "jpeg", "png", "gif", "webp", "heic", "bmp", "tif", "tiff", "psd", "svg", "raw", "cr2", "nef", "ico"},
	"archive": {"zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz", "iso", "img", "vhd", "vhdx", "vmdk", "wim", "cab", "dmg", "nupkg", "asar", "pack"},
	"doc":     {"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "md", "csv", "rtf", "odt", "epub"},
	"code":    {"js", "ts", "tsx", "jsx", "py", "go", "rs", "java", "c", "cpp", "h", "cs", "json", "html", "css", "map", "log", "sql", "yml", "yaml", "toml", "lock", "ipynb"},
	"exe":     {"exe", "dll", "sys", "msi", "node", "so", "dylib", "bin", "lib", "pdb", "obj", "o", "a"},
}

var extType = func() map[string]string {
	m := map[string]string{}
	for typ, exts := range typeExts {
		for _, e := range exts {
			m[e] = typ
		}
	}
	return m
}()

// typeOrder is the fixed order of the colour classes: slot 0 is "other", the rest follow the palette order.
var typeOrder = []string{"other", "video", "archive", "image", "doc", "audio", "code", "exe"}

var typeIndex = func() map[string]int {
	m := map[string]int{}
	for i, t := range typeOrder {
		m[t] = i
	}
	return m
}()

func typeIdxOf(name string) int { return typeIndex[fileTypeOf(name)] }

// dominant returns the class that holds the most bytes and its share of the total (0 if there are no bytes).
func dominant(t [8]int64) (name string, share float64) {
	var total, best int64
	bi := 0
	for i, v := range t {
		total += v
		if v > best {
			best, bi = v, i
		}
	}
	if total <= 0 {
		return "", 0
	}
	return typeOrder[bi], float64(best) / float64(total)
}

// fileTypeOf puts a file name in one of the map's colour classes.
func fileTypeOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return "other"
	}
	if t, ok := extType[strings.ToLower(name[i+1:])]; ok {
		return t
	}
	return "other"
}
