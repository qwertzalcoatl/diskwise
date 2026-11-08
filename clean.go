package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type cleanOptions struct {
	costly    bool
	rules     map[string]bool
	olderThan time.Duration
	minSize   int64
	apply     bool
	yes       bool
}

type planItem struct {
	rule   *Rule
	path   string // "" for Command items
	size   int64
	newest int64
	count  int
	bin    string
}

// protected paths are never deleted or emptied, whatever a rule says.
var protected = []string{"~", "~/Library", "~/Library/Application Support", "~/Library/Containers",
	"~/Library/Group Containers", "~/Library/Caches", "~/Documents", "~/Desktop",
	"~/Pictures", "~/Movies", "~/Music", "~/Downloads", "~/.ssh", "~/.gnupg", "~/.config",
	"~/.local", "~/.cache"}

// guard re-checks a path right before it is touched: it must still exist,
// be a real directory inside home, not be a top-level home folder or protected
// location, and still match the same rule (the scan may be stale).

func buildPlan(s *Scan, o cleanOptions) []planItem {
	cutoff := int64(0)
	if o.olderThan > 0 {
		cutoff = time.Now().Add(-o.olderThan).Unix()
	}
	var items []planItem
	for _, g := range groupMatches(s) {
		r := g.Rule
		if r.Action == None || r.Safety > Costly || (r.Safety == Costly && !o.costly && !o.rules[r.ID]) {
			continue
		}
		if len(o.rules) > 0 && !o.rules[r.ID] {
			continue
		}
		if r.Action == Command {
			if g.Size >= o.minSize {
				items = append(items, planItem{rule: r, size: g.Size, count: len(g.Matches), bin: r.Binary()})
			}
			continue
		}
		for _, m := range g.Matches {
			if m.Size < o.minSize || (cutoff > 0 && m.Newest > cutoff) {
				continue
			}
			if !strings.HasPrefix(m.Path, homeDir+"/") || isProtected(m.Path) || (filepath.Dir(m.Path) == homeDir && r.Action != DeleteContents) {
				continue
			}
			items = append(items, planItem{rule: r, path: m.Path, size: m.Size, newest: m.Newest, count: 1})
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].size > items[j].size })
	return items
}

func (it planItem) what() string {
	if it.rule.Action == Command {
		argv := append([]string{}, it.rule.Command...)
		s := "$ " + strings.Join(argv, " ")
		if it.bin == "" {
			s += "  (not installed, skipped)"
		}
		return s
	}
	verb := "delete"
	if it.rule.Action == DeleteContents {
		verb = "empty "
	}
	return verb + " " + display(it.path)
}

func runClean(s *Scan, o cleanOptions) error {
	items := buildPlan(s, o)
	if len(items) == 0 {
		fmt.Println("nothing to clean with these options (try --costly, or `diskwise report`)")
		return nil
	}
	var total int64
	fmt.Printf("\n\033[1mPLAN\033[0m (from scan %s ago)\n", time.Since(s.StartedAt).Round(time.Minute))
	for _, it := range items {
		total += it.size
		fmt.Printf("  %s %8s  %5s  %-14s %s\n", it.rule.Safety.Icon(), human(it.size), ago(it.newest), it.rule.ID,
			truncMid(it.what(), 90))
	}
	fmt.Printf("  ─ up to %s in %d actions\n", human(total), len(items))
	if !o.apply {
		fmt.Println("\nDry run. Nothing was changed. Re-run with --apply to go through the items one by one")
		fmt.Println("(--yes skips the questions). Filters: --rule id,id  --older-than 30d  --min-size 100M  --costly")
		return nil
	}
	if len(s.Volume.Snapshots) > 0 {
		fmt.Printf("\n⚠ %d Time Machine local snapshots exist; deleted data stays on disk until they expire.\n",
			len(s.Volume.Snapshots))
	}

	m := newMatcher()
	in := bufio.NewReader(os.Stdin)
	before := freeBytes(homeDir)
	all := o.yes
	for _, it := range items {
		if it.rule.Action == Command && it.bin == "" {
			continue
		}
		if !all {
			fmt.Printf("\n%s %s  %s\n", it.rule.Safety.Icon(), human(it.size), it.what())
			if it.rule.Note != "" {
				fmt.Printf("  %s\n", it.rule.Note)
			}
			fmt.Print("  run? [y]es [n]o [a]ll remaining [q]uit: ")
			ans, _ := in.ReadString('\n')
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "y", "yes":
			case "a", "all":
				all = true
			case "q", "quit":
				goto done
			default:
				continue
			}
		}
		if err := execute(it, m); err != nil {
			fmt.Printf("  ✗ %v\n", err)
			logAction(it, err)
		} else {
			fmt.Printf("  ✓ %s\n", it.what())
			logAction(it, nil)
		}
	}
done:
	freed := freeBytes(homeDir) - before
	fmt.Printf("\nfree space changed by %s (APFS may take a moment to reclaim; log: %s)\n", human(max(freed, 0)),
		display(actionLogPath()))
	return nil
}

func execute(it planItem, m *matcher) error {
	r := it.rule
	if r.Action == Command {
		cmd := exec.Command(it.bin, r.Command[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	if err := guard(it.path, r, m); err != nil {
		return err
	}
	if r.Action == Delete {
		return removeAll(it.path)
	}
	es, err := os.ReadDir(it.path)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range es {
		if err := removeAll(filepath.Join(it.path, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// guard re-checks a path right before it is touched: it must still exist,
// be a real directory inside home, not be a protected location, and still
// match the same rule (the scan may be stale).
func guard(p string, r *Rule, m *matcher) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("refusing unclean path %q", p)
	}
	if !strings.HasPrefix(p, homeDir+"/") {
		return fmt.Errorf("refusing %s: outside your home folder", display(p))
	}
	for _, q := range protected {
		if p == expand(q) {
			return fmt.Errorf("refusing protected location %s", display(p))
		}
	}
	if filepath.Dir(p) == homeDir && r.Action != DeleteContents {
		return fmt.Errorf("refusing top-level home directory %s", display(p))
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("%s: %w", display(p), unwrap(err))
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("refusing %s: not a plain directory", display(p))
	}
	if real, err := filepath.EvalSymlinks(p); err != nil || logical(real) != p {
		return fmt.Errorf("refusing %s: resolves to %s", display(p), real)
	}
	var names map[string]bool
	has := func(n string) bool {
		if names == nil {
			names = map[string]bool{}
			if es, err := os.ReadDir(filepath.Dir(p)); err == nil {
				for _, e := range es {
					names[e.Name()] = true
				}
			}
		}
		return names[n]
	}
	if got := m.match(p, has); got != r {
		return fmt.Errorf("refusing %s: no longer matches rule %s", display(p), r.ID)
	}
	return nil
}

// removeAll is os.RemoveAll that also handles read-only directories
// (e.g. Go module caches, some node_modules).
func removeAll(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	_ = filepath.WalkDir(p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(q, 0o755)
		}
		return nil
	})
	return os.RemoveAll(p)
}

func actionLogPath() string { return filepath.Join(stateDir("state"), "actions.log") }

func logAction(it planItem, err error) {
	f, e := os.OpenFile(actionLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if e != nil {
		return
	}
	defer f.Close()
	rec := map[string]any{"time": time.Now().Format(time.RFC3339), "rule": it.rule.ID, "size": it.size,
		"action": it.what(), "ok": err == nil}
	if err != nil {
		rec["error"] = err.Error()
	}
	_ = json.NewEncoder(f).Encode(rec)
}
