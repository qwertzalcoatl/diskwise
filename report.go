package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Bucket is one slice of a disjoint partition of the scanned tree: a
// directory with no large child, or the leftover of a directory whose large
// children are listed separately.
type Bucket struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Files  int64  `json:"files"`
	Newest int64  `json:"newest"`
	Rest   bool   `json:"rest,omitempty"` // leftover of Path after its listed children
	Label  string `json:"label"`
	Known  bool   `json:"known"`
}

func buckets(n *Node, min int64) []Bucket {
	if n == nil || n.Size < min {
		return nil
	}
	var out []Bucket
	var covered int64
	for _, c := range n.Children {
		if c.Size >= min {
			out = append(out, buckets(c, min)...)
			covered += c.Size
		}
	}
	if len(out) == 0 {
		return []Bucket{{Path: n.Path, Size: n.Size, Files: n.Files, Newest: n.Newest}}
	}
	if rest := n.Size - covered; rest >= min {
		out = append(out, Bucket{Path: n.Path, Size: rest, Newest: n.Newest, Rest: true})
	}
	return out
}

func within(path, dir string) bool {
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

// label explains a bucket using the rule matches of the scan.
func label(b *Bucket, s *Scan, min int64) {
	for _, m := range s.Matches {
		if !b.Rest && within(b.Path, m.Path) {
			r := ruleByID[m.Rule]
			if r == nil {
				continue
			}
			b.Label, b.Known = r.Safety.Icon()+" "+r.Name, true
			return
		}
	}
	type part struct {
		name string
		size int64
	}
	sums := map[string]int64{}
	var total int64
	for _, m := range s.Matches {
		if ruleByID[m.Rule] == nil || m.Path == b.Path || !within(m.Path, b.Path) {
			continue
		}
		if b.Rest && listedChild(s, b, m.Path, min) {
			continue // counted in a child's own bucket
		}
		sums[m.Rule] += m.Size
		total += m.Size
	}
	if total*4 >= b.Size && total > 0 {
		var parts []part
		for id, sz := range sums {
			parts = append(parts, part{id, sz})
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i].size > parts[j].size })
		var ss []string
		for i, p := range parts {
			if i == 2 {
				ss = append(ss, "…")
				break
			}
			r := ruleByID[p.name]
			ss = append(ss, fmt.Sprintf("%s %s %s", r.Safety.Icon(), r.Name, human(p.size)))
		}
		b.Label, b.Known = strings.Join(ss, ", "), true
		return
	}
	b.Label = "? unknown"
}

// listedChild reports whether path lies under a child of b that has its own
// bucket (only relevant for leftover buckets).
func listedChild(s *Scan, b *Bucket, path string, min int64) bool {
	n := s.Tree.find(b.Path)
	if n == nil {
		return false
	}
	for _, c := range n.Children {
		if c.Size >= min && within(path, c.Path) {
			return true
		}
	}
	return false
}

type RuleGroup struct {
	Rule    *Rule
	Matches []Match
	Size    int64
	Stale   int // untouched for 90+ days
}

func groupMatches(s *Scan) []*RuleGroup {
	by := map[string]*RuleGroup{}
	staleBefore := time.Now().Add(-90 * 24 * time.Hour).Unix()
	for _, m := range s.Matches {
		r := ruleByID[m.Rule]
		if r == nil {
			continue
		}
		g := by[m.Rule]
		if g == nil {
			g = &RuleGroup{Rule: r}
			by[m.Rule] = g
		}
		g.Matches = append(g.Matches, m)
		g.Size += m.Size
		if m.Newest > 0 && m.Newest < staleBefore {
			g.Stale++
		}
	}
	var gs []*RuleGroup
	for _, g := range by {
		gs = append(gs, g)
	}
	sort.Slice(gs, func(i, j int) bool { return gs[i].Size > gs[j].Size })
	return gs
}

type reportOptions struct {
	min    int64
	rule   string
	asJSON bool
	limit  int
}

func autoMin(s *Scan) int64 {
	m := s.Volume.Used / 100
	if m < 1e9 {
		m = 1e9
	}
	return m
}

func computeBuckets(s *Scan, min int64) []Bucket {
	bs := buckets(s.Tree, min)
	for i := range bs {
		label(&bs[i], s, min)
	}
	sort.Slice(bs, func(i, j int) bool { return bs[i].Size > bs[j].Size })
	return bs
}

func printReport(w io.Writer, s *Scan, o reportOptions) {
	min := o.min
	if min <= 0 {
		min = autoMin(s)
	}
	bs := computeBuckets(s, min)
	groups := groupMatches(s)

	if o.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"volume": s.Volume, "scanned": s.Tree.Size, "buckets": bs,
			"rules": groups, "bigFiles": s.BigFiles, "errors": s.Errors})
		return
	}
	if o.rule != "" {
		printRuleDetail(w, groups, o.rule)
		return
	}

	v := s.Volume
	fmt.Fprintf(w, "\n\033[1mdiskwise\033[0m · %s · scanned %s ago in %s\n\n", display(logical(s.Root)),
		time.Since(s.StartedAt).Round(time.Minute), time.Duration(s.Seconds*float64(time.Second)).Round(time.Second))
	fmt.Fprintf(w, "\033[1mVOLUME\033[0m  capacity %s · used %s · free %s", human(v.Capacity), human(v.Used),
		human(v.Available))
	if o := v.Others(); o > 1e9 {
		fmt.Fprintf(w, " · other volumes/reserved %s", human(o))
	}
	if v.Available < v.Capacity/20 {
		fmt.Fprintf(w, "  \033[31m⚠ under 5%% free\033[0m")
	}
	fmt.Fprintln(w)
	hidden := v.Used - s.Tree.Size
	fmt.Fprintf(w, "        scan saw %s in %s files", human(s.Tree.Size), commas(s.Tree.Files))
	if hidden < -1e9 {
		fmt.Fprintf(w, " · %s more than used: APFS clones share blocks, so deleting a clone frees less than its size",
			human(-hidden))
	}
	if hidden > 1e9 {
		fmt.Fprintf(w, " · %s not seen", human(hidden))
		var why []string
		if s.Errors > 0 {
			why = append(why, fmt.Sprintf("%s unreadable paths", commas(s.Errors)))
		}
		if len(v.Snapshots) > 0 {
			why = append(why, fmt.Sprintf("%d local snapshots", len(v.Snapshots)))
		}
		why = append(why, "purgeable/system data")
		fmt.Fprintf(w, " (%s)", strings.Join(why, ", "))
	}
	fmt.Fprintln(w)
	if len(v.Snapshots) > 0 {
		fmt.Fprintf(w, "        ⚠ %d Time Machine local snapshots hold deleted data: `tmutil listlocalsnapshots /`\n", len(v.Snapshots))
	}

	fmt.Fprintf(w, "\n\033[1mBIGGEST BUCKETS\033[0m (≥ %s, disjoint)\n", human(min))
	limit := o.limit
	if limit <= 0 {
		limit = 30
	}
	var unknown []Bucket
	for i, b := range bs {
		if i >= limit {
			fmt.Fprintf(w, "  … %d more (use --limit)\n", len(bs)-limit)
			break
		}
		p := display(b.Path)
		if b.Rest {
			p += "/ (other)"
		}
		fmt.Fprintf(w, "  %8s  %-58s  %5s  %s\n", human(b.Size), truncMid(p, 58), ago(b.Newest), b.Label)
		if !b.Known && !b.Rest {
			unknown = append(unknown, b)
		}
	}

	fmt.Fprintf(w, "\n\033[1mRECOGNISED\033[0m (by rule)\n")
	var tot [4]int64
	for _, g := range groups {
		r := g.Rule
		tot[r.Safety] += g.Size
		if g.Size < 100e6 {
			continue
		}
		count := fmt.Sprintf("%d", len(g.Matches))
		if g.Stale > 0 {
			count += fmt.Sprintf(" (%d stale)", g.Stale)
		}
		act := r.Describe()
		if r.Action == Command && r.Binary() == "" {
			act += " (not installed)"
		}
		fmt.Fprintf(w, "  %s %8s  %-36s %-14s %-18s %s\n", r.Safety.Icon(), human(g.Size), r.Name, count, r.ID, act)
	}
	fmt.Fprintf(w, "  ─ %s regenerable · %s costly to rebuild · %s user data · %s system\n",
		human(tot[Regenerable]), human(tot[Costly]), human(tot[UserData]), human(tot[System]))

	if len(s.BigFiles) > 0 {
		fmt.Fprintf(w, "\n\033[1mLARGEST FILES\033[0m (≥ 1 GB)\n")
		for i, f := range s.BigFiles {
			if i == 10 {
				fmt.Fprintf(w, "  … %d more\n", len(s.BigFiles)-10)
				break
			}
			fmt.Fprintf(w, "  %8s  %-70s  %5s\n", human(f.Size), truncMid(display(f.Path), 70), ago(f.Mtime))
		}
	}

	fmt.Fprintln(w, "\n\033[1mNEXT\033[0m")
	if len(unknown) > 0 {
		fmt.Fprintf(w, "  diskwise explain --unknown       ask the LLM about the %d unknown buckets\n", len(unknown))
	}
	fmt.Fprintln(w, "  diskwise report --rule <id>      list every match of a rule")
	fmt.Fprintln(w, "  diskwise select                  interactively check items with SPACE & delete")
	fmt.Fprintln(w, "  diskwise clean                   dry run: what can be freed safely")
	if s.Errors > 0 {
		fmt.Fprintf(w, "  (%s paths were unreadable; grant your terminal Full Disk Access for a complete scan)\n", commas(s.Errors))
	}
	fmt.Fprintln(w)
}

func printRuleDetail(w io.Writer, groups []*RuleGroup, id string) {
	for _, g := range groups {
		if g.Rule.ID != id {
			continue
		}
		r := g.Rule
		fmt.Fprintf(w, "\n%s %s — %s in %d places · %s · action: %s\n", r.Safety.Icon(), r.Name, human(g.Size),
			len(g.Matches), r.Safety, r.Describe())
		if r.Note != "" {
			fmt.Fprintf(w, "  %s\n", r.Note)
		}
		fmt.Fprintln(w)
		for _, m := range g.Matches {
			fmt.Fprintf(w, "  %8s  %5s  %s\n", human(m.Size), ago(m.Newest), display(m.Path))
		}
		fmt.Fprintln(w)
		return
	}
	if ruleByID[id] == nil {
		fmt.Fprintf(os.Stderr, "unknown rule %q; see `diskwise rules`\n", id)
		return
	}
	fmt.Fprintf(w, "no matches for %s in the last scan\n", id)
}

func printRules(w io.Writer) {
	cat := ""
	for _, r := range catalog {
		if r.Category != cat {
			cat = r.Category
			fmt.Fprintf(w, "\n\033[1m%s\033[0m\n", cat)
		}
		pats := make([]string, len(r.Patterns))
		for i, p := range r.Patterns {
			pats[i] = p
			if p == "*" {
				pats[i] = "any directory containing " + cacheDirTag
			}
			if r.Children {
				pats[i] += "/*"
			}
		}
		fmt.Fprintf(w, "  %s %-22s %-30s %s\n", r.Safety.Icon(), r.ID, r.Describe(), strings.Join(pats, ", "))
	}
	fmt.Fprintln(w, "\n🟢 regenerable  🟡 costly to rebuild  🔴 user data (report only)  ⛔ system (report only)")
}
