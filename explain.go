package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Facts is the metadata about a directory that is sent to the LLM. It never
// includes file contents.
type Facts struct {
	Path        string           `json:"path"`
	Size        int64            `json:"size"`
	Files       int64            `json:"files"`
	Dirs        int64            `json:"dirs"`
	Sampled     bool             `json:"sampled"`
	Entries     []string         `json:"entries"`
	TopChildren []kv             `json:"topChildren"`
	TopExts     []kv             `json:"topExtensions"`
	Ages        map[string]int64 `json:"bytesByAge"`
	Largest     []kv             `json:"largestFiles"`
}

type kv struct {
	K string `json:"name"`
	V int64  `json:"bytes"`
}

const gatherBudget = 20 * time.Second

func topN(m map[string]int64, n int) []kv {
	var out []kv
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].V > out[j].V })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func gatherFacts(path string) (*Facts, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	f := &Facts{Path: display(path), Ages: map[string]int64{}}
	if es, err := os.ReadDir(path); err == nil {
		for i, e := range es {
			if i == 40 {
				f.Entries = append(f.Entries, fmt.Sprintf("… %d more", len(es)-40))
				break
			}
			n := e.Name()
			if e.IsDir() {
				n += "/"
			}
			f.Entries = append(f.Entries, n)
		}
	}
	children := map[string]int64{}
	exts := map[string]int64{}
	var largest []kv
	now := time.Now()
	deadline := now.Add(gatherBudget)
	seen := map[[2]uint64]bool{}

	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			f.Sampled = true
			return filepath.SkipAll
		}
		if p == path {
			return nil
		}
		var s syscall.Stat_t
		if syscall.Lstat(p, &s) != nil {
			return nil
		}
		if d.IsDir() {
			f.Dirs++
			return nil
		}
		if s.Nlink > 1 {
			k := [2]uint64{uint64(s.Dev), s.Ino}
			if seen[k] {
				return nil
			}
			seen[k] = true
		}
		sz := s.Blocks * 512
		f.Size += sz
		f.Files++
		rel, _ := filepath.Rel(path, p)
		first := strings.SplitN(rel, "/", 2)[0]
		children[first] += sz
		ext := strings.ToLower(filepath.Ext(p))
		if ext == "" || len(ext) > 12 {
			ext = "(none)"
		}
		exts[ext] += sz
		age := now.Sub(time.Unix(s.Mtimespec.Sec, 0))
		switch {
		case age < 7*24*time.Hour:
			f.Ages["<7d"] += sz
		case age < 30*24*time.Hour:
			f.Ages["7-30d"] += sz
		case age < 180*24*time.Hour:
			f.Ages["1-6mo"] += sz
		case age < 365*24*time.Hour:
			f.Ages["6-12mo"] += sz
		default:
			f.Ages[">1y"] += sz
		}
		if sz > 50e6 {
			largest = append(largest, kv{rel, sz})
		}
		return nil
	})
	f.TopChildren = topN(children, 12)
	f.TopExts = topN(exts, 10)
	sort.Slice(largest, func(i, j int) bool { return largest[i].V > largest[j].V })
	if len(largest) > 8 {
		largest = largest[:8]
	}
	f.Largest = largest
	return f, nil
}

func (f *Facts) prompt() string {
	var b strings.Builder
	b.WriteString(`You help a macOS developer free disk space. Identify what created the directory below and whether
it can be deleted. Use only the metadata given (no file contents are available). Reply with a single JSON object,
no prose, with these keys:
  "what":       one sentence: which app or tool owns this and what it stores
  "category":   one of cache, build-output, dependencies, vm-or-container, ai-model, backup, app-data, user-files, system, unknown
  "safety":     one of regenerable (rebuilt automatically), costly (re-downloadable/rebuildable but slow),
                user-data (only the user can decide), system (managed by macOS)
  "action":     one short concrete recommendation; prefer the owning tool's own cleanup command or UI
  "command":    a shell command for that action, or "" if a UI action or manual review is better
  "confidence": number 0-1

`)
	fmt.Fprintf(&b, "Directory: %s\nAllocated size: %s in %d files, %d subdirectories", f.Path, human(f.Size), f.Files, f.Dirs)
	if f.Sampled {
		b.WriteString(" (walk stopped early; numbers are a lower bound)")
	}
	b.WriteString("\nEntries: " + strings.Join(f.Entries, ", "))
	b.WriteString("\nLargest children: ")
	for i, c := range f.TopChildren {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", c.K, human(c.V))
	}
	b.WriteString("\nBytes by extension: ")
	for i, c := range f.TopExts {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", c.K, human(c.V))
	}
	b.WriteString("\nBytes by last modification: ")
	for _, k := range []string{"<7d", "7-30d", "1-6mo", "6-12mo", ">1y"} {
		fmt.Fprintf(&b, "%s %s; ", k, human(f.Ages[k]))
	}
	if len(f.Largest) > 0 {
		b.WriteString("\nLargest files: ")
		for i, c := range f.Largest {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s %s", c.K, human(c.V))
		}
	}
	b.WriteString("\n")
	return b.String()
}

type Verdict struct {
	What       string  `json:"what"`
	Category   string  `json:"category"`
	Safety     string  `json:"safety"`
	Action     string  `json:"action"`
	Command    string  `json:"command"`
	Confidence float64 `json:"confidence"`
}

func parseVerdict(s string) (*Verdict, error) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return nil, fmt.Errorf("no JSON in reply: %q", truncLeft(strings.TrimSpace(s), 200))
	}
	var v Verdict
	if err := json.Unmarshal([]byte(s[i:j+1]), &v); err != nil {
		return nil, fmt.Errorf("bad JSON in reply: %w", err)
	}
	return &v, nil
}

type cachedVerdict struct {
	Verdict Verdict   `json:"verdict"`
	Size    int64     `json:"size"`
	At      time.Time `json:"at"`
	Model   string    `json:"model"`
}

func explainCachePath() string { return filepath.Join(stateDir("cache"), "explain.json") }

func loadExplainCache() map[string]cachedVerdict {
	m := map[string]cachedVerdict{}
	if b, err := os.ReadFile(explainCachePath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveExplainCache(m map[string]cachedVerdict) {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(explainCachePath(), b, 0o644)
}

type explainOptions struct {
	paths   []string
	unknown bool
	fresh   bool
	dry     bool
	min     int64
}

func runExplain(o explainOptions, scanFile string) error {
	paths := o.paths
	if o.unknown {
		s, err := loadScan(scanFile)
		if err != nil {
			return err
		}
		min := o.min
		if min <= 0 {
			min = autoMin(s)
		}
		for _, b := range computeBuckets(s, min) {
			if !b.Known && !b.Rest {
				paths = append(paths, b.Path)
			}
		}
		if len(paths) == 0 {
			fmt.Println("no unknown buckets in the last scan")
			return nil
		}
	}
	if len(paths) == 0 {
		return fmt.Errorf("usage: diskwise explain [--unknown] [--fresh] [--dry] <path>...")
	}

	var llm LLM
	if !o.dry {
		var err error
		if llm, err = pickLLM(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "using %s · only metadata (names, sizes, dates) is sent, never file contents\n", llm.Name())
	}
	cache := loadExplainCache()
	m := newMatcher()
	for _, p := range paths {
		p = expand(p)
		if r := m.match(p, nil); r != nil && !o.fresh {
			fmt.Printf("\n\033[1m%s\033[0m\n  %s known rule %s (%s): %s\n", display(p), r.Safety.Icon(), r.ID, r.Name,
				or(r.Note, r.Describe()))
			continue
		}
		fmt.Fprintf(os.Stderr, "gathering facts for %s …\n", display(p))
		f, err := gatherFacts(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  skip: %v\n", err)
			continue
		}
		if o.dry {
			fmt.Printf("\n----- prompt for %s -----\n%s", display(p), f.prompt())
			continue
		}
		key := display(p)
		if c, ok := cache[key]; ok && !o.fresh && within10(c.Size, f.Size) {
			printVerdict(p, f, &c.Verdict, "cached "+c.At.Format("2006-01-02"))
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		reply, err := llm.Ask(ctx, f.prompt())
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "  LLM error: %v\n", err)
			continue
		}
		v, err := parseVerdict(reply)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %v\n", err)
			continue
		}
		cache[key] = cachedVerdict{*v, f.Size, time.Now(), llm.Name()}
		saveExplainCache(cache)
		printVerdict(p, f, v, llm.Name())
	}
	if !o.dry {
		fmt.Println("\ndiskwise never runs LLM-suggested commands. Review them before running anything.")
	}
	return nil
}

func within10(a, b int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d*10 <= b
}

func printVerdict(p string, f *Facts, v *Verdict, src string) {
	icon := map[string]string{"regenerable": "🟢", "costly": "🟡", "user-data": "🔴", "system": "⛔"}[v.Safety]
	if icon == "" {
		icon = "❔"
	}
	fmt.Printf("\n\033[1m%s\033[0m  %s  (%s, confidence %.0f%%, %s)\n", display(p), human(f.Size), v.Category,
		v.Confidence*100, src)
	fmt.Printf("  %s %s — %s\n", icon, v.Safety, v.What)
	fmt.Printf("  → %s\n", v.Action)
	if v.Command != "" {
		fmt.Printf("    $ %s\n", v.Command)
	}
}
