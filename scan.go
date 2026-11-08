package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Node is a directory in the size tree. Only directories at least
// Scan.KeepMin large are kept; their small descendants are folded in.
type Node struct {
	Path     string  `json:"p"`
	Size     int64   `json:"s"` // allocated bytes, hard links counted once
	Files    int64   `json:"f"`
	Newest   int64   `json:"m"` // newest file mtime (unix)
	Children []*Node `json:"c,omitempty"`
}

// Match is a directory recognised by a catalog rule.
type Match struct {
	Rule   string `json:"rule"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Files  int64  `json:"files"`
	Newest int64  `json:"newest"`
}

type BigFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

type Scan struct {
	Version      int        `json:"version"`
	Root         string     `json:"root"`
	StartedAt    time.Time  `json:"startedAt"`
	Seconds      float64    `json:"seconds"`
	KeepMin      int64      `json:"keepMin"`
	Volume       VolumeInfo `json:"volume"`
	Tree         *Node      `json:"tree"`
	Matches      []Match    `json:"matches"`
	BigFiles     []BigFile  `json:"bigFiles"`
	Errors       int64      `json:"errors"`
	ErrorSamples []string   `json:"errorSamples,omitempty"`
}

const bigFileMin = 1e9

type walker struct {
	dev      int32
	keepMin  int64
	excludes map[string]bool
	match    *matcher
	sem      chan struct{}
	seen     sync.Map // [dev, ino] of multiply-linked files

	files, bytes, errs atomic.Int64

	mu         sync.Mutex
	matches    []Match
	bigFiles   []BigFile
	errSamples []string
}

type dirStat struct {
	size, files, newest int64
	node                *Node
	tagged              bool // contains CACHEDIR.TAG
}

func (w *walker) fail(p string, err error) {
	w.errs.Add(1)
	w.mu.Lock()
	if len(w.errSamples) < 25 {
		w.errSamples = append(w.errSamples, fmt.Sprintf("%s: %v", display(logical(p)), unwrap(err)))
	}
	w.mu.Unlock()
}

func unwrap(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err
	}
	return err
}

// walkDir returns the totals of a directory. inMatch stops nested rule
// matches (no node_modules inside node_modules).
func (w *walker) walkDir(real string, inMatch bool) dirStat {
	var st dirStat
	entries, err := os.ReadDir(real)
	if err != nil {
		w.fail(real, err)
	}
	for _, e := range entries {
		if e.Name() == cacheDirTag {
			st.tagged = true
			if !inMatch {
				inMatch = true // the parent records this dir as a match
			}
			break
		}
	}
	var names map[string]bool
	parentHas := func(n string) bool {
		if names == nil {
			names = make(map[string]bool, len(entries))
			for _, e := range entries {
				names[e.Name()] = true
			}
		}
		return names[n]
	}

	type sub struct {
		real string
		rule *Rule
	}
	var subs []sub
	for _, e := range entries {
		p := real + "/" + e.Name()
		if real == "/" {
			p = "/" + e.Name()
		}
		var s syscall.Stat_t
		if err := syscall.Lstat(p, &s); err != nil {
			w.fail(p, err)
			continue
		}
		alloc := s.Blocks * 512
		if s.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			if s.Dev != w.dev || w.excludes[p] {
				continue // another volume, or excluded
			}
			st.size += alloc
			var r *Rule
			if !inMatch {
				r = w.match.matchNamed(logical(p), parentHas)
			}
			subs = append(subs, sub{p, r})
			continue
		}
		if s.Nlink > 1 {
			if _, dup := w.seen.LoadOrStore([2]uint64{uint64(s.Dev), s.Ino}, struct{}{}); dup {
				continue
			}
		}
		st.size += alloc
		st.files++
		if s.Mtimespec.Sec > st.newest {
			st.newest = s.Mtimespec.Sec
		}
		w.files.Add(1)
		w.bytes.Add(alloc)
		if alloc >= bigFileMin {
			w.mu.Lock()
			w.bigFiles = append(w.bigFiles, BigFile{logical(p), alloc, s.Mtimespec.Sec})
			w.mu.Unlock()
		}
	}

	results := make([]dirStat, len(subs))
	var wg sync.WaitGroup
	for i, sd := range subs {
		select {
		case w.sem <- struct{}{}:
			wg.Add(1)
			go func(i int, sd sub) {
				defer wg.Done()
				results[i] = w.walkDir(sd.real, inMatch || sd.rule != nil)
				<-w.sem
			}(i, sd)
		default:
			results[i] = w.walkDir(sd.real, inMatch || sd.rule != nil)
		}
	}
	wg.Wait()

	var kids []*Node
	for i, r := range results {
		st.size += r.size
		st.files += r.files
		if r.newest > st.newest {
			st.newest = r.newest
		}
		if r.node != nil {
			kids = append(kids, r.node)
		}
		rule := subs[i].rule
		if rule == nil && r.tagged && !inMatch {
			rule = ruleByID["cachedir-tag"]
		}
		if rule != nil && r.size > 0 {
			w.mu.Lock()
			w.matches = append(w.matches, Match{rule.ID, logical(subs[i].real), r.size, r.files, r.newest})
			w.mu.Unlock()
		}
	}
	if st.size >= w.keepMin {
		sort.Slice(kids, func(a, b int) bool { return kids[a].Size > kids[b].Size })
		st.node = &Node{Path: logical(real), Size: st.size, Files: st.files, Newest: st.newest, Children: kids}
	}
	return st
}

type scanOptions struct {
	root     string
	keepMin  int64
	excludes []string
	workers  int
	progress bool
}

func defaultRoot() string {
	if fi, err := os.Stat(dataPrefix); err == nil && fi.IsDir() {
		return dataPrefix
	}
	return "/"
}

func runScan(o scanOptions) (*Scan, error) {
	root := filepath.Clean(o.root)
	if strings.HasPrefix(root, "~") {
		root = filepath.Join(homeDir, strings.TrimPrefix(root[1:], "/"))
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	var rs syscall.Stat_t
	if err := syscall.Stat(root, &rs); err != nil {
		return nil, fmt.Errorf("cannot scan %s: %w", root, err)
	}
	w := &walker{dev: rs.Dev, keepMin: o.keepMin, match: newMatcher(), sem: make(chan struct{}, o.workers),
		excludes: map[string]bool{}}
	for _, e := range o.excludes {
		e = expand(e)
		w.excludes[e] = true
		if root == dataPrefix || strings.HasPrefix(root, dataPrefix+"/") {
			w.excludes[dataPrefix+e] = true
		}
	}

	start := time.Now()
	done := make(chan struct{})
	if o.progress {
		go func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					fmt.Fprint(os.Stderr, "\r\033[K")
					return
				case <-t.C:
					fmt.Fprintf(os.Stderr, "\r\033[Kscanning %s … %s files, %s, %d unreadable, %s",
						display(logical(root)), commas(w.files.Load()), human(w.bytes.Load()), w.errs.Load(),
						time.Since(start).Round(time.Second))
				}
			}
		}()
	}
	st := w.walkDir(root, false)
	close(done)

	if st.node == nil {
		st.node = &Node{Path: logical(root), Size: st.size, Files: st.files, Newest: st.newest}
	}
	sort.Slice(w.matches, func(a, b int) bool { return w.matches[a].Size > w.matches[b].Size })
	sort.Slice(w.bigFiles, func(a, b int) bool { return w.bigFiles[a].Size > w.bigFiles[b].Size })
	return &Scan{
		Version: 1, Root: root, StartedAt: start, Seconds: time.Since(start).Seconds(), KeepMin: o.keepMin,
		Volume: volumeInfo(root), Tree: st.node, Matches: w.matches, BigFiles: w.bigFiles,
		Errors: w.errs.Load(), ErrorSamples: w.errSamples,
	}, nil
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func defaultScanFile() string { return filepath.Join(stateDir("cache"), "scan.json") }

func saveScan(s *Scan, path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(s); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadScan(path string) (*Scan, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no scan found at %s; run `diskwise scan` first", display(path))
	}
	if err != nil {
		return nil, err
	}
	var s Scan
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s is not a diskwise scan: %w", path, err)
	}
	return &s, nil
}

// find returns the deepest kept node at or above path.
func (n *Node) find(path string) *Node {
	if n == nil || !(n.Path == path || n.Path == "/" || strings.HasPrefix(path, n.Path+"/")) {
		return nil
	}
	for _, c := range n.Children {
		if f := c.find(path); f != nil {
			return f
		}
	}
	return n
}
