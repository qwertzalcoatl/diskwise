package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMatcher(t *testing.T) {
	m := newMatcher()
	has := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, x := range names {
				if x == n {
					return true
				}
			}
			return false
		}
	}
	h := homeDir
	cases := []struct {
		path   string
		parent func(string) bool
		want   string
	}{
		{h + "/Xcode/DerivedData", nil, "xcode-derived"},
		{h + "/Library/Caches/Homebrew", nil, "homebrew-cache"}, // specific beats the children rule
		{h + "/Library/Caches/com.spotify.client", nil, "user-caches"},
		{h + "/Library/Caches", nil, ""}, // the cache root itself is not a match
		{h + "/.cache/huggingface", nil, "huggingface"},
		{h + "/.cache/pre-commit", nil, "dot-cache"},
		{h + "/projects/app/node_modules", nil, "node-modules"},
		{h + "/projects/rs/target", has("Cargo.toml"), "cargo-target"},
		{h + "/projects/java/target", has("pom.xml"), ""},
		{h + "/projects/pkg/.build", has("Package.swift"), "swiftpm-build"},
		{h + "/.tart/vms/test-vm", nil, "tart-vms"},
		{h + "/.tart/cache", nil, "tart-cache"},
		{h + "/projects/x/.claude/worktrees/agent-1", nil, "agent-worktrees"},
		{h + "/Library/Application Support/Code/Cache", nil, "electron-cache"},
		{h + "/projects/site/Cache", nil, ""}, // Cond: only under Application Support
		{h + "/.claude/projects", nil, "claude-transcripts"},
	}
	for _, c := range cases {
		got := ""
		if r := m.match(c.path, c.parent); r != nil {
			got = r.ID
		}
		if got != c.want {
			t.Errorf("match(%s) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestBucketsPartition(t *testing.T) {
	g := int64(1e9)
	tree := &Node{Path: "/", Size: 100 * g, Children: []*Node{
		{Path: "/vol", Size: 80 * g, Children: []*Node{
			{Path: "/vol/work", Size: 75 * g, Children: []*Node{
				{Path: "/vol/work/big", Size: 50 * g},
				{Path: "/vol/work/small", Size: 5 * g},
			}},
		}},
		{Path: "/opt", Size: 5 * g},
	}}
	bs := buckets(tree, 10*g)
	var sum int64
	got := map[string]int64{}
	for _, b := range bs {
		sum += b.Size
		k := b.Path
		if b.Rest {
			k += "*"
		}
		got[k] = b.Size
	}
	want := map[string]int64{"/vol/work/big": 50 * g, "/vol/work*": 25 * g, "/*": 20 * g}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("bucket %s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if sum > tree.Size {
		t.Errorf("buckets overlap: %d > %d", sum, tree.Size)
	}
}

func TestGuard(t *testing.T) {
	m := newMatcher()
	dir, err := os.MkdirTemp(homeDir, ".diskwise-test-")
	if err != nil {
		t.Skip("cannot create a temp dir in home:", err)
	}
	defer os.RemoveAll(dir)
	dir = expand(dir)
	nm := filepath.Join(dir, "node_modules")
	_ = os.Mkdir(nm, 0o755)
	r := ruleByID["node-modules"]
	if err := guard(nm, r, m); err != nil {
		t.Errorf("guard(node_modules) = %v, want ok", err)
	}
	if err := guard(dir, r, m); err == nil {
		t.Error("guard accepted a dir that does not match the rule")
	}
	link := filepath.Join(dir, "x", "node_modules")
	_ = os.Mkdir(filepath.Dir(link), 0o755)
	_ = os.Symlink(nm, link)
	if err := guard(link, r, m); err == nil {
		t.Error("guard accepted a symlink")
	}
	if err := guard(homeDir, r, m); err == nil {
		t.Error("guard accepted home")
	}
	if err := guard("/tmp/node_modules", r, m); err == nil {
		t.Error("guard accepted a path outside home")
	}
}

func TestScanSmallTree(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "app")
	_ = os.MkdirAll(filepath.Join(app, "node_modules", "left-pad", "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(app, "node_modules", "left-pad", "index.js"), make([]byte, 100_000), 0o644)
	_ = os.WriteFile(filepath.Join(app, "main.js"), make([]byte, 10_000), 0o644)
	// a hard link must be counted once
	_ = os.Link(filepath.Join(app, "main.js"), filepath.Join(app, "main-link.js"))
	// a self-declared cache; the node_modules inside must not be listed separately
	tc := filepath.Join(dir, "custom-target")
	_ = os.MkdirAll(filepath.Join(tc, "debug", "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(tc, cacheDirTag), []byte("Signature: 8a477f597d28d172789f06886806bc55"), 0o644)

	s, err := runScan(scanOptions{root: dir, keepMin: 0, workers: 4})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tree.Files != 3 {
		t.Errorf("files = %d, want 3 (hard link counted once)", s.Tree.Files)
	}
	got := map[string]string{}
	for _, m := range s.Matches {
		got[filepath.Base(m.Path)] = m.Rule
	}
	want := map[string]string{"node_modules": "node-modules", "custom-target": "cachedir-tag"}
	if len(got) != len(want) || got["node_modules"] != want["node_modules"] || got["custom-target"] != want["custom-target"] {
		t.Fatalf("matches = %+v, want %v", s.Matches, want)
	}
	for _, m := range s.Matches {
		if m.Rule == "node-modules" && m.Size < 100_000 {
			t.Errorf("node_modules size = %d, want ≥ 100kB", m.Size)
		}
	}
	// guard-side matching (full matcher) must agree with the scanner
	if r := newMatcher().match(expand(tc), nil); r == nil || r.ID != "cachedir-tag" {
		t.Errorf("match(custom-target) = %v, want cachedir-tag", r)
	}
}

func TestBuildSelectList(t *testing.T) {
	now := time.Now().Unix()
	old := time.Now().Add(-60 * 24 * time.Hour).Unix()
	s := &Scan{
		Matches: []Match{
			{Rule: "node-modules", Path: homeDir + "/projects/app1/node_modules", Size: 200e6, Newest: now},
			{Rule: "node-modules", Path: homeDir + "/projects/app2/node_modules", Size: 500e6, Newest: old},
			{Rule: "tart-vms", Path: homeDir + "/.tart/vms/vm1", Size: 50e9, Newest: old},
			{Rule: "vm-swap", Path: "/private/var/vm", Size: 2e9, Newest: now},
			{Rule: "downloads", Path: homeDir + "/Downloads", Size: 5e9, Newest: old}, // protected path!
			{Rule: "user-caches", Path: "/tmp/outside-home", Size: 300e6, Newest: old},
		},
	}
	def := buildSelectList(s, selectOptions{minSize: 1e6})
	if len(def) != 2 || def[0].size != 500e6 || def[1].size != 200e6 {
		t.Fatalf("default select list = %+v, want 2 node_modules sorted by size descending", def)
	}
	// --older-than filter
	stale := buildSelectList(s, selectOptions{minSize: 1e6, olderThan: 30 * 24 * time.Hour})
	if len(stale) != 1 || stale[0].path != homeDir+"/projects/app2/node_modules" {
		t.Fatalf("olderThan select list = %+v, want only app2/node_modules", stale)
	}
	// --rule filter (explicitly naming a 🔴 rule includes it)
	byRule := buildSelectList(s, selectOptions{minSize: 1e6, rules: map[string]bool{"tart-vms": true}})
	if len(byRule) != 1 || byRule[0].rule.ID != "tart-vms" {
		t.Fatalf("rule-filtered select list = %+v, want only tart-vms", byRule)
	}
	// --all includes 🔴 rules inside home, but still excludes ⛔ system, outside-home, and protected ~/Downloads
	all := buildSelectList(s, selectOptions{allRules: true, minSize: 1e6})
	if len(all) != 3 {
		t.Fatalf("allRules select list = %+v, want 3 items (tart-vms + 2 node_modules)", all)
	}
}

func TestSelectStateKeysAndRender(t *testing.T) {
	items := []planItem{
		{rule: ruleByID["node-modules"], path: homeDir + "/projects/a/node_modules", size: 300e6},
		{rule: ruleByID["cargo-registry"], path: homeDir + "/.cargo/registry", size: 200e6},
		{rule: ruleByID["tart-vms"], path: homeDir + "/.tart/vms/vm1", size: 100e6},
	}
	st := newSelectState(items)

	// Space checks item 0
	if proceed, cancel := st.handleKey([]byte{' '}); proceed || cancel || !st.checked[0] {
		t.Fatalf("space on 0: proceed=%v cancel=%v checked=%v", proceed, cancel, st.checked)
	}
	// Down arrow moves cursor to 1; space checks item 1
	st.handleKey([]byte{27, '[', 'B'})
	if st.cursor != 1 {
		t.Fatalf("down arrow: cursor=%d, want 1", st.cursor)
	}
	st.handleKey([]byte{' '})
	sel, total := st.selectedItems()
	if len(sel) != 2 || total != 500e6 {
		t.Fatalf("selected = %d (%d B), want 2 (500 MB)", len(sel), total)
	}
	// 'k' moves back to 0; 'j' moves to 1
	st.handleKey([]byte{'k'})
	if st.cursor != 0 {
		t.Fatalf("k: cursor=%d, want 0", st.cursor)
	}
	st.handleKey([]byte{'j'})
	if st.cursor != 1 {
		t.Fatalf("j: cursor=%d, want 1", st.cursor)
	}
	// 'a' checks all since item 2 was unchecked, then 'a' again unchecks all
	st.handleKey([]byte{'a'})
	if !st.checked[0] || !st.checked[1] || !st.checked[2] {
		t.Fatalf("'a' should check all: %v", st.checked)
	}
	st.handleKey([]byte{'a'})
	if st.checked[0] || st.checked[1] || st.checked[2] {
		t.Fatalf("second 'a' should uncheck all: %v", st.checked)
	}
	// 'g' toggles only 🟢 regenerable items (item 0 is 🟢, item 1 is 🟡, item 2 is 🔴)
	st.handleKey([]byte{'g'})
	if !st.checked[0] || st.checked[1] || st.checked[2] {
		t.Fatalf("'g' should check only 🟢 item 0: %v", st.checked)
	}
	// Render output reflects checked state and summary
	var out bytes.Buffer
	st.renderTo(&out, 24, 100)
	rendered := out.String()
	if !strings.Contains(rendered, "1 checked (300 MB)") || !strings.Contains(rendered, "[✓]") {
		t.Errorf("render output missing checked summary or [✓]:\n%s", rendered)
	}
	// Enter returns proceed=true
	if proceed, cancel := st.handleKey([]byte{'\r'}); !proceed || cancel {
		t.Errorf("enter: proceed=%v cancel=%v, want true/false", proceed, cancel)
	}
	// 'q' returns cancel=true
	if proceed, cancel := st.handleKey([]byte{'q'}); proceed || !cancel {
		t.Errorf("q: proceed=%v cancel=%v, want false/true", proceed, cancel)
	}
}

func TestConfirmAndExecuteWarnings(t *testing.T) {
	dir, err := os.MkdirTemp(homeDir, ".diskwise-test-select-")
	if err != nil {
		t.Skip("cannot create a temp dir in home:", err)
	}
	defer os.RemoveAll(dir)
	dir = expand(dir)

	nm := filepath.Join(dir, "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "pkg.js"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	selected := []planItem{{rule: ruleByID["node-modules"], path: nm, size: 5}}

	// 1. Wrong confirmation word -> aborts before countdown, directory stays intact
	var out bytes.Buffer
	err = confirmAndExecute(selected, 5, 2, time.Millisecond, strings.NewReader("yes\n"), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Please confirm you want this: Write CONFIRM:") ||
		!strings.Contains(out.String(), "Aborted (did not write CONFIRM)") {
		t.Errorf("unexpected output on bad confirm:\n%s", out.String())
	}
	if _, err := os.Stat(nm); err != nil {
		t.Fatalf("node_modules was deleted despite not writing CONFIRM: %v", err)
	}

	// 2. Lowercase "confirm" -> also rejected (must be exact "CONFIRM")
	out.Reset()
	_ = confirmAndExecute(selected, 5, 2, time.Millisecond, strings.NewReader("confirm\n"), &out, nil)
	if _, err := os.Stat(nm); err != nil {
		t.Fatalf("node_modules was deleted on lowercase confirm: %v", err)
	}

	// 3. Exact "CONFIRM", but interrupted during the countdown -> aborts, directory stays intact
	out.Reset()
	abortCh := make(chan os.Signal, 1)
	abortCh <- syscall.SIGINT
	err = confirmAndExecute(selected, 5, 3, 50*time.Millisecond, strings.NewReader("CONFIRM\n"), &out, abortCh)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WARNING 2/2") || !strings.Contains(out.String(), "Aborted during countdown") {
		t.Errorf("expected countdown abort message, got:\n%s", out.String())
	}
	if _, err := os.Stat(nm); err != nil {
		t.Fatalf("node_modules was deleted despite countdown abort: %v", err)
	}

	// 4. Exact "CONFIRM" + full countdown -> deletes the directory
	out.Reset()
	err = confirmAndExecute(selected, 5, 3, time.Millisecond, strings.NewReader("CONFIRM\n"), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Deleting in 3 second(s)...") ||
		!strings.Contains(out.String(), "Deleting in 1 second(s)...") ||
		!strings.Contains(out.String(), "✓ delete") {
		t.Errorf("expected full countdown and deletion output, got:\n%s", out.String())
	}
	if _, err := os.Stat(nm); !os.IsNotExist(err) {
		t.Fatalf("expected node_modules to be deleted after CONFIRM + countdown, stat err = %v", err)
	}
}
