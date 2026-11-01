package main

// Categories say how risky a duplicate is to remove: your own files, versus things an app or a build owns.
// They are data, not code: each one has a plain-language definition and a list of rules, and the user can edit them.
//
// A rule is matched case-insensitively against the full path (always written with backslashes):
//   text            the path contains this text            \downloads\
//   a*b             * matches any run of characters         node_modules*\.bin
//   ^text           the path starts with this               ^c:\temp\
//   text$           the path ends with this                 .vhdx$
//   {home}          your profile folder                     {home}\documents\
//   {otherusers}    any other account's profile folder
//   re:expression   a regular expression (Go/RE2 syntax)    re:^[a-z]:\\te?mp\\
// The first category (top to bottom) with a matching rule wins. A category with no rules matches everything,
// so put it last as the catch-all.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Category struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Risk  string   `json:"risk"` // low | medium | high | review
	Desc  string   `json:"desc"`
	Rules []string `json:"rules"`
}

func defaultCategories() []Category {
	return []Category{
		{ID: "other-users", Name: "Other user accounts", Risk: "high",
			Desc:  "Files inside another Windows account's profile folder. They belong to that account, so removing them needs that person's say-so (and usually admin rights).",
			Rules: []string{"{otherusers}"}},
		{ID: "downloads", Name: "Downloads", Risk: "low",
			Desc:  "Your Downloads folder. Usually the safest place to clean: things you fetched and can fetch again, typically installers saved twice as \"name (1).exe\".",
			Rules: []string{`\downloads\`}},
		{ID: "temp", Name: "Temp folders", Risk: "medium",
			Desc:  "Temporary folders (AppData\\Local\\Temp, C:\\Temp, C:\\Tmp). Meant to be disposable, but running programs keep working files here, so close the apps first. Other Claude, Cursor or editor sessions keep their scratch space here too.",
			Rules: []string{`\appdata\local\temp\`, `re:^[a-z]:\\te?mp\\`}},
		{ID: "dev", Name: "Dev & build output", Risk: "medium",
			Desc:  "Folders that tools generate and can rebuild: node_modules, virtual environments, build output, caches and git worktrees. Deleting one copy is safe only if you are ready to reinstall or rebuild; the project can be broken until you do. Beware hardlinks: removing a \"copy\" may free nothing.",
			Rules: []string{"node_modules", `\.gocache`, `\.gotmp`, "go-build", `\build\`, `\.claude\worktrees`, ".worktrees", `\wt-`, `\.next\`, `\.venv\`, "site-packages", `\.gradle`, `\.cache\`, `\sdk-cache`, `\android-apk-cache`, `\.expo\`}},
		{ID: "apps", Name: "App data & installed apps", Risk: "high",
			Desc:  "Where installed programs keep their files (AppData, ProgramData, Program Files). A duplicate here is usually how the app was built (an update next to the old version, a shared DLL). Removing one can break the app. Leave it alone unless you are uninstalling the app.",
			Rules: []string{`\appdata\`, `re:^[a-z]:\\programdata\\`, `\program files`, `\.codex\`}},
		{ID: "other", Name: "Other files (documents, projects, backups)", Risk: "review",
			Desc:  "Everything that matched none of the rules above: your documents, projects, backups and media. These are your own files, so look at each group yourself.",
			Rules: []string{}},
	}
}

type matcher func(lowerPath string) bool

type Classifier struct {
	Cats []Category
	m    [][]matcher
}

func newClassifier(cats []Category, user string) (*Classifier, error) {
	c := &Classifier{Cats: cats}
	profile := os.Getenv("USERPROFILE")
	users := []string{strings.ToLower(user)}
	if user == "" {
		users = []string{strings.ToLower(filepath.Base(profile))}
		// the same folder can also be written in its old 8.3 form (RUNNER~1 for runneradmin), as %TEMP% often is
		if short := strings.ToLower(shortName(profile)); short != "" && short != users[0] {
			users = append(users, short)
		}
	}
	home := strings.ToLower(profile)
	for _, cat := range cats {
		var ms []matcher
		for _, r := range cat.Rules {
			m, err := compileRule(r, users, home)
			if err != nil {
				return nil, fmt.Errorf("%s: %q: %w", cat.Name, r, err)
			}
			ms = append(ms, m)
		}
		c.m = append(c.m, ms)
	}
	return c, nil
}

func compileRule(rule string, users []string, home string) (matcher, error) {
	rule = strings.TrimSpace(rule)
	switch {
	case rule == "":
		return nil, fmt.Errorf("empty rule")
	case rule == "{otherusers}":
		return func(p string) bool {
			// ?:\users\<name>\...  where <name> is not the current account (under either of its spellings)
			if len(p) <= 9 || p[1:9] != `:\users\` {
				return false
			}
			for _, u := range users {
				if strings.HasPrefix(p[9:], u+`\`) {
					return false
				}
			}
			return true
		}, nil
	case strings.HasPrefix(rule, "re:"):
		rx, err := regexp.Compile("(?i)" + rule[3:])
		if err != nil {
			return nil, err
		}
		return rx.MatchString, nil
	}
	rule = strings.ReplaceAll(strings.ReplaceAll(rule, "/", `\`), "{home}", home)
	rule = strings.ToLower(rule)
	anchoredStart, anchoredEnd := strings.HasPrefix(rule, "^"), strings.HasSuffix(rule, "$")
	rule = strings.TrimSuffix(strings.TrimPrefix(rule, "^"), "$")
	if rule == "" {
		return nil, fmt.Errorf("empty rule")
	}
	parts := strings.Split(rule, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	expr := strings.Join(parts, ".*")
	if anchoredStart {
		expr = "^" + expr
	}
	if anchoredEnd {
		expr += "$"
	}
	rx, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	return rx.MatchString, nil
}

// Category returns the index of the first matching category, or -1 if none matches.
func (c *Classifier) Category(path string) int {
	p := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	for i, ms := range c.m {
		if len(ms) == 0 {
			return i
		}
		for _, m := range ms {
			if m(p) {
				return i
			}
		}
	}
	return -1
}

// validateCategories rejects what the editor must not save: no categories, blank names, bad rules.
func validateCategories(cats []Category) error {
	if len(cats) == 0 {
		return fmt.Errorf("at least one category is needed")
	}
	seen := map[string]bool{}
	for i := range cats {
		cats[i].Name = strings.TrimSpace(cats[i].Name)
		if cats[i].Name == "" {
			return fmt.Errorf("category %d has no name", i+1)
		}
		if cats[i].ID == "" || seen[cats[i].ID] {
			cats[i].ID = fmt.Sprintf("c%d-%s", i+1, strings.ToLower(strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
					return r
				}
				return '-'
			}, cats[i].Name)))
		}
		seen[cats[i].ID] = true
		switch cats[i].Risk {
		case "low", "medium", "high", "review":
		default:
			cats[i].Risk = "review"
		}
		rules := []string{} // never nil: a nil slice would be sent to the window as null
		for _, r := range cats[i].Rules {
			if r = strings.TrimSpace(r); r != "" {
				rules = append(rules, r)
			}
		}
		cats[i].Rules = rules
	}
	_, err := newClassifier(cats, "")
	return err
}

func loadCategories(path string) []Category {
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultCategories()
	}
	var cats []Category
	if json.Unmarshal(b, &cats) != nil || validateCategories(cats) != nil {
		return defaultCategories()
	}
	return cats
}

func saveCategories(path string, cats []Category) error {
	b, err := json.MarshalIndent(cats, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
