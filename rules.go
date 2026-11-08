package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Safety says what deleting a match costs.
type Safety int

const (
	Regenerable Safety = iota // rebuilt automatically, cheap
	Costly                    // re-downloadable or rebuildable, but slow
	UserData                  // only the user can decide
	System                    // managed by macOS, never touched
)

func (s Safety) Icon() string { return [...]string{"🟢", "🟡", "🔴", "⛔"}[s] }
func (s Safety) String() string {
	return [...]string{"regenerable", "costly", "user-data", "system"}[s]
}

// Action is what `diskwise clean` does with a match.
type Action int

const (
	None           Action = iota // report only
	Delete                       // remove the matched directory
	DeleteContents               // empty the matched directory, keep it
	Command                      // run the tool's own cleanup command once
)

// Rule recognises a directory by path. Patterns are logical paths with ~
// expanded; a leading "*/" means "any directory whose path ends with this".
// With Children set, the rule matches each direct child of a pattern match
// instead of the match itself (so each app cache is listed on its own).
type Rule struct {
	ID       string
	Name     string
	Category string
	Patterns []string
	Children bool
	// Cond refines a match. parentHas reports whether the parent directory
	// contains an entry with the given name.
	Cond    func(path string, parentHas func(string) bool) bool
	Safety  Safety
	Action  Action
	Command []string // argv for Action == Command
	// Bins are fallback locations for Command[0] when it is not on PATH.
	Bins []string
	Note string
}

func (r *Rule) Describe() string {
	switch r.Action {
	case Delete:
		return "delete"
	case DeleteContents:
		return "empty"
	case Command:
		return strings.Join(r.Command, " ")
	}
	return "review"
}

// Binary resolves Command[0], or "" when the tool is not installed.
func (r *Rule) Binary() string {
	if len(r.Command) == 0 {
		return ""
	}
	if p, err := exec.LookPath(r.Command[0]); err == nil {
		return p
	}
	for _, b := range r.Bins {
		b = expand(b)
		if fi, err := os.Stat(b); err == nil && !fi.IsDir() {
			return b
		}
	}
	return ""
}

func parentHasFile(name string) func(string, func(string) bool) bool {
	return func(_ string, has func(string) bool) bool { return has(name) }
}

func containsFile(name string) func(string, func(string) bool) bool {
	return func(p string, _ func(string) bool) bool {
		_, err := os.Lstat(filepath.Join(p, name))
		return err == nil
	}
}

func under(prefix string) func(string, func(string) bool) bool {
	prefix = expand(prefix) + "/"
	return func(p string, _ func(string) bool) bool { return strings.HasPrefix(p, prefix) }
}

// catalog is ordered: when several rules match a directory, the first wins.
// Specific paths therefore come before the broad "children of a cache dir"
// rules at the end.
var catalog = []*Rule{
	// --- Xcode / Apple development ---------------------------------------
	{ID: "xcode-derived", Name: "Xcode DerivedData", Category: "Xcode",
		Patterns: []string{"*/Xcode/DerivedData"},
		Safety:   Regenerable, Action: DeleteContents, Note: "rebuilt on next build"},
	{ID: "xcode-archives", Name: "Xcode Archives", Category: "Xcode",
		Patterns: []string{"*/Xcode/Archives"},
		Safety:   UserData, Note: "keeps dSYMs for symbolicating shipped builds; prune old ones in Xcode Organizer"},
	{ID: "xcode-device-support", Name: "Xcode device support", Category: "Xcode",
		Patterns: []string{"*/Xcode/iOS DeviceSupport", "*/Xcode/watchOS DeviceSupport",
			"*/Xcode/tvOS DeviceSupport", "*/Xcode/visionOS DeviceSupport",
			"*/Xcode/macOS DeviceSupport"},
		Children: true, Safety: Regenerable, Action: Delete, Note: "re-copied when a device with that OS connects"},
	{ID: "sim-devices", Name: "Simulator devices (unavailable)", Category: "Xcode",
		Patterns: []string{"*/CoreSimulator/Devices"},
		Safety:   Costly, Action: Command, Command: []string{"xcrun", "simctl", "delete", "unavailable"},
		Note: "only removes simulators whose runtime is gone; delete others in Xcode ▸ Devices"},
	{ID: "sim-caches", Name: "Simulator caches", Category: "Xcode",
		Patterns: []string{"*/CoreSimulator/Caches"},
		Safety:   Regenerable, Action: DeleteContents},
	{ID: "sim-runtimes", Name: "Simulator runtimes", Category: "Xcode",
		Patterns: []string{"*/CoreSimulator/Images", "*/CoreSimulator/Cryptex"},
		Safety:   Costly, Action: Command, Command: []string{"xcrun", "simctl", "runtime", "delete", "--unusable"},
		Note: "removes unusable runtimes; remove others in Xcode ▸ Settings ▸ Components"},
	{ID: "swiftpm-build", Name: "SwiftPM .build", Category: "Build output",
		Patterns: []string{"*/.build"}, Cond: parentHasFile("Package.swift"),
		Safety: Regenerable, Action: Delete, Note: "rebuilt by swift build"},

	// --- VMs & containers --------------------------------------------------
	{ID: "docker-desktop", Name: "Docker Desktop disk", Category: "VMs & containers",
		Patterns: []string{"*/com.docker.docker/Data/vms"},
		Safety:   Costly, Action: Command, Command: []string{"docker", "system", "prune", "-f"},
		Note: "removes stopped containers, dangling images, unused networks; add -a --volumes by hand for more"},
	{ID: "podman-machine", Name: "Podman machine disk", Category: "VMs & containers",
		Patterns: []string{"*/containers/podman/machine"},
		Safety:   Costly, Action: Command, Command: []string{"podman", "system", "prune", "-f"},
		Note: "prunes inside the running machine; the disk image may not shrink right away"},
	{ID: "orbstack", Name: "OrbStack data", Category: "VMs & containers",
		Patterns: []string{"*/.orbstack/data"},
		Safety:   Costly, Note: "prune with `docker system prune` inside OrbStack"},
	{ID: "lima", Name: "Lima/Colima VMs", Category: "VMs & containers",
		Patterns: []string{"*/.lima", "*/.colima"},
		Safety:   UserData, Note: "delete unused instances with limactl delete / colima delete"},
	{ID: "tart-cache", Name: "Tart image cache", Category: "VMs & containers",
		Patterns: []string{"*/.tart/cache"},
		Safety:   Costly, Action: Command, Command: []string{"tart", "prune", "--entries=caches", "--older-than=0"},
		Note: "base images, re-pulled when a VM is cloned again"},
	{ID: "tart-vms", Name: "Tart VMs", Category: "VMs & containers",
		Patterns: []string{"*/.tart/vms"}, Children: true,
		Safety: UserData, Note: "each VM is a full macOS/Linux install; `tart delete <name>` if unused"},
	{ID: "utm", Name: "UTM VMs", Category: "VMs & containers",
		Patterns: []string{"*/com.utmapp.UTM/Data/Documents"}, Children: true,
		Safety: UserData},
	{ID: "parallels", Name: "Parallels VMs", Category: "VMs & containers",
		Patterns: []string{"*/Parallels"}, Children: true, Safety: UserData},
	{ID: "android-avd", Name: "Android emulators", Category: "VMs & containers",
		Patterns: []string{"*/.android/avd"}, Children: true, Safety: UserData},

	// --- JavaScript ----------------------------------------------------------
	{ID: "node-modules", Name: "node_modules", Category: "Dependencies",
		Patterns: []string{"*/node_modules"},
		Safety:   Regenerable, Action: Delete, Note: "reinstall with npm/pnpm/yarn install"},
	{ID: "npm-cache", Name: "npm cache", Category: "Package caches",
		Patterns: []string{"*/.npm/_cacache"},
		Safety:   Regenerable, Action: Command, Command: []string{"npm", "cache", "clean", "--force"}},
	{ID: "pnpm-store", Name: "pnpm store", Category: "Package caches",
		Patterns: []string{"*/pnpm/store", "*/.pnpm-store"},
		Safety:   Regenerable, Action: Command, Command: []string{"pnpm", "store", "prune"}},
	{ID: "bun-cache", Name: "Bun cache", Category: "Package caches",
		Patterns: []string{"*/.bun/install/cache"}, Safety: Regenerable, Action: DeleteContents},
	{ID: "next-cache", Name: "Next.js .next", Category: "Build output",
		Patterns: []string{"*/.next"}, Cond: parentHasFile("package.json"),
		Safety: Regenerable, Action: Delete},

	// --- Python --------------------------------------------------------------
	{ID: "python-venv", Name: "Python virtualenvs", Category: "Dependencies",
		Patterns: []string{"*/.venv", "*/venv", "*/.virtualenv"}, Cond: containsFile("pyvenv.cfg"),
		Safety: Regenerable, Action: Delete, Note: "recreate from requirements/pyproject"},
	{ID: "uv-cache", Name: "uv cache", Category: "Package caches",
		Patterns: []string{"*/.cache/uv"}, Safety: Regenerable, Action: Command,
		Command: []string{"uv", "cache", "clean"}},
	{ID: "conda-pkgs", Name: "Conda package cache", Category: "Package caches",
		Patterns: []string{"*/miniconda3/pkgs", "*/anaconda3/pkgs", "*/miniforge3/pkgs", "*/mambaforge/pkgs"},
		Safety:   Regenerable, Action: Command, Command: []string{"conda", "clean", "--all", "-y"}},
	{ID: "pyenv", Name: "pyenv Python versions", Category: "Toolchains",
		Patterns: []string{"*/.pyenv/versions"}, Children: true, Safety: Costly,
		Note: "pyenv uninstall <version>"},

	// --- Rust / Go / JVM -----------------------------------------------------
	{ID: "cargo-target", Name: "Cargo target/", Category: "Build output",
		Patterns: []string{"*/target"}, Cond: parentHasFile("Cargo.toml"),
		Safety: Regenerable, Action: Delete, Note: "rebuilt by cargo build"},
	{ID: "cargo-registry", Name: "Cargo registry", Category: "Package caches",
		Patterns: []string{"*/.cargo/registry"}, Safety: Costly, Action: DeleteContents,
		Note: "crates are re-downloaded on next build"},
	{ID: "rustup", Name: "Rust toolchains", Category: "Toolchains",
		Patterns: []string{"*/.rustup/toolchains"}, Children: true, Safety: Costly,
		Note: "rustup toolchain uninstall <name>"},
	{ID: "go-build", Name: "Go build cache", Category: "Package caches",
		Patterns: []string{"*/go-build"}, Safety: Regenerable, Action: Command,
		Command: []string{"go", "clean", "-cache"}},
	{ID: "go-mod", Name: "Go module cache", Category: "Package caches",
		Patterns: []string{"*/go/pkg/mod"}, Safety: Costly, Action: Command,
		Command: []string{"go", "clean", "-modcache"}},
	{ID: "gradle", Name: "Gradle caches", Category: "Package caches",
		Patterns: []string{"*/.gradle/caches", "*/.gradle/wrapper/dists"}, Safety: Costly, Action: DeleteContents},
	{ID: "maven", Name: "Maven repository", Category: "Package caches",
		Patterns: []string{"*/.m2/repository"}, Safety: Costly, Action: DeleteContents},
	{ID: "android-sdk", Name: "Android SDK system images", Category: "Toolchains",
		Patterns: []string{"*/Android/sdk/system-images"}, Children: true, Safety: Costly,
		Note: "remove in Android Studio ▸ SDK Manager"},

	// --- Homebrew ------------------------------------------------------------
	{ID: "homebrew-cache", Name: "Homebrew downloads", Category: "Package caches",
		Patterns: []string{"*/Caches/Homebrew"}, Safety: Regenerable, Action: Command,
		Command: []string{"brew", "cleanup", "-s", "--prune=all"}},

	// --- AI models -----------------------------------------------------------
	{ID: "ollama", Name: "Ollama models", Category: "AI models",
		Patterns: []string{"*/.ollama/models"}, Safety: Costly, Note: "`ollama list`, then `ollama rm <model>`"},
	{ID: "huggingface", Name: "Hugging Face cache", Category: "AI models",
		Patterns: []string{"*/.cache/huggingface"}, Safety: Costly, Note: "`hf cache scan`, then `hf cache delete`"},
	{ID: "lmstudio", Name: "LM Studio models", Category: "AI models",
		Patterns: []string{"*/.lmstudio/models", "*/.cache/lm-studio/models"}, Safety: Costly,
		Note: "delete models in LM Studio ▸ My Models"},

	// --- Agent tooling ---------------------------------------------------------
	{ID: "agent-worktrees", Name: "Agent git worktrees", Category: "Agent tooling",
		Patterns: []string{"*/.claude/worktrees", "*/.codex/worktrees", "*/.cursor/worktrees"}, Children: true,
		Safety: UserData, Note: "may hold uncommitted work; remove finished ones with `git worktree remove`"},
	{ID: "claude-transcripts", Name: "Claude Code transcripts", Category: "Agent tooling",
		Patterns: []string{"*/.claude/projects"}, Safety: UserData,
		Note: "session history; Claude Code prunes it after cleanupPeriodDays (settings.json, default 30)"},

	// --- Apple apps & user data ------------------------------------------------
	{ID: "ios-backups", Name: "iPhone/iPad backups", Category: "Backups",
		Patterns: []string{"*/MobileSync/Backup"}, Children: true,
		Safety: UserData, Note: "manage in Finder ▸ device ▸ Manage Backups"},
	{ID: "mail", Name: "Mail", Category: "Apple apps",
		Patterns: []string{"~/Library/Mail"}, Safety: UserData},
	{ID: "messages", Name: "Messages attachments", Category: "Apple apps",
		Patterns: []string{"*/Messages/Attachments"}, Safety: UserData,
		Note: "System Settings ▸ General ▸ Storage ▸ Messages"},
	{ID: "photos", Name: "Photos library", Category: "Apple apps",
		Patterns: []string{"*/Photos Library.photoslibrary"}, Safety: UserData,
		Note: "Photos ▸ Settings ▸ iCloud ▸ Optimize Mac Storage"},
	{ID: "icloud", Name: "iCloud Drive (local)", Category: "Apple apps",
		Patterns: []string{"~/Library/Mobile Documents"}, Safety: UserData,
		Note: "right-click ▸ Remove Download keeps files in iCloud"},
	{ID: "downloads", Name: "Downloads", Category: "User files",
		Patterns: []string{"~/Downloads"}, Safety: UserData},
	{ID: "trash", Name: "Trash", Category: "User files",
		Patterns: []string{"~/.Trash"}, Safety: Costly, Action: DeleteContents,
		Note: "permanent: files in the Trash cannot be restored afterwards"},

	// --- Broad caches (keep last) ------------------------------------------------
	{ID: "electron-cache", Name: "App web caches (Electron/Chromium)", Category: "App caches",
		Patterns: []string{"*/Cache", "*/Code Cache", "*/GPUCache", "*/CachedData", "*/DawnGraphiteCache",
			"*/DawnWebGPUCache", "*/Service Worker/CacheStorage"},
		Cond:   under("~/Library/Application Support"),
		Safety: Regenerable, Action: DeleteContents, Note: "quit the app first"},
	{ID: "app-logs", Name: "App logs", Category: "App caches",
		Patterns: []string{"~/Library/Logs"}, Children: true, Safety: Regenerable, Action: Delete},
	{ID: "user-caches", Name: "App caches", Category: "App caches",
		Patterns: []string{"~/Library/Caches"}, Children: true, Safety: Regenerable, Action: Delete,
		Note: "quit the app first; it rebuilds its cache"},
	{ID: "dot-cache", Name: "Tool caches (~/.cache)", Category: "App caches",
		Patterns: []string{"~/.cache"}, Children: true, Safety: Regenerable, Action: Delete},
	// Matched by content, not name: any directory holding a CACHEDIR.TAG
	// (https://bford.info/cachedir/) declares itself a regenerable cache.
	// Cargo target dirs (also custom CARGO_TARGET_DIRs), ccache, pip... write one.
	{ID: "cachedir-tag", Name: "Tagged caches (CACHEDIR.TAG)", Category: "App caches",
		Patterns: []string{"*"}, Cond: containsFile(cacheDirTag),
		Safety: Regenerable, Action: Delete, Note: "the owning tool marked it as a cache it can rebuild"},
}

const cacheDirTag = "CACHEDIR.TAG"

var ruleByID = func() map[string]*Rule {
	m := map[string]*Rule{}
	for _, r := range catalog {
		m[r.ID] = r
	}
	return m
}()

// matcher indexes the catalog so each directory is checked in O(few).
type matcher struct {
	exact      map[string][]int // path -> rule indexes (self)
	childExact map[string][]int // parent path -> rule indexes (children)
	suffix     map[string][]int // basename -> rule indexes with */ patterns (self)
	childSuf   map[string][]int // parent basename -> rule indexes with */ patterns (children)
	sufOf      map[[2]int]string
	any        []int // "*" rules: content-based, checked last (costs a stat)
}

func newMatcher() *matcher {
	m := &matcher{exact: map[string][]int{}, childExact: map[string][]int{}, suffix: map[string][]int{},
		childSuf: map[string][]int{}, sufOf: map[[2]int]string{}}
	for i, r := range catalog {
		for j, p := range r.Patterns {
			if p == "*" {
				m.any = append(m.any, i)
				continue
			}
			if strings.HasPrefix(p, "*/") {
				suf := p[1:] // "/node_modules"
				m.sufOf[[2]int{i, j}] = suf
				base := filepath.Base(suf)
				if r.Children {
					m.childSuf[base] = append(m.childSuf[base], i)
				} else {
					m.suffix[base] = append(m.suffix[base], i)
				}
				continue
			}
			p = expand(p)
			if r.Children {
				m.childExact[p] = append(m.childExact[p], i)
			} else {
				m.exact[p] = append(m.exact[p], i)
			}
		}
	}
	return m
}

func (m *matcher) hasSuffix(i int, path string) bool {
	for j := range catalog[i].Patterns {
		if suf, ok := m.sufOf[[2]int{i, j}]; ok && strings.HasSuffix(path, suf) {
			return true
		}
	}
	return false
}

// match returns the first catalog rule for a directory, or nil.
// path is logical; parentHas may be nil (treated as "no").
func (m *matcher) match(path string, parentHas func(string) bool) *Rule {
	if r := m.matchNamed(path, parentHas); r != nil {
		return r
	}
	for _, i := range m.any {
		if r := catalog[i]; r.Cond == nil || r.Cond(path, parentHas) {
			return r
		}
	}
	return nil
}

// matchNamed is match without the content-based "*" rules. The scanner uses
// it and detects those rules from the directory listing it reads anyway.
func (m *matcher) matchNamed(path string, parentHas func(string) bool) *Rule {
	if parentHas == nil {
		parentHas = func(string) bool { return false }
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	best := -1
	consider := func(i int) {
		if best != -1 && i >= best {
			return
		}
		r := catalog[i]
		if r.Cond != nil && !r.Cond(path, parentHas) {
			return
		}
		best = i
	}
	for _, i := range m.exact[path] {
		consider(i)
	}
	for _, i := range m.childExact[parent] {
		consider(i)
	}
	for _, i := range m.suffix[name] {
		if m.hasSuffix(i, path) {
			consider(i)
		}
	}
	for _, i := range m.childSuf[filepath.Base(parent)] {
		if m.hasSuffix(i, parent) {
			consider(i)
		}
	}
	if best == -1 {
		return nil
	}
	return catalog[best]
}
