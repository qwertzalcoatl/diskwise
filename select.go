package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type selectOptions struct {
	allRules  bool // include 🔴 user-data rules too (still never ⛔ system)
	rules     map[string]bool
	olderThan time.Duration
	minSize   int64
	countdown int
}

// buildSelectList returns recognized items for interactive selection.
// By default it includes 🟢 regenerable and 🟡 costly items; with allRules
// it also includes 🔴 user-data items (deleting their matched directory).
// ⛔ system items and protected paths are never included.
func buildSelectList(s *Scan, o selectOptions) []planItem {
	cutoff := int64(0)
	if o.olderThan > 0 {
		cutoff = time.Now().Add(-o.olderThan).Unix()
	}
	var items []planItem
	for _, g := range groupMatches(s) {
		r := g.Rule
		if r.Safety == System {
			continue
		}
		if r.Safety == UserData && !o.allRules && !o.rules[r.ID] {
			continue
		}
		if len(o.rules) > 0 && !o.rules[r.ID] {
			continue
		}
		if r.Action == Command {
			bin := r.Binary()
			if bin != "" && g.Size >= o.minSize {
				items = append(items, planItem{rule: r, size: g.Size, count: len(g.Matches), bin: bin})
			}
			continue
		}
		// For rules with Action == None (review-only), only allow selecting individual
		// directories that are inside homeDir and not in the protected list.
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

func isProtected(p string) bool {
	for _, q := range protected {
		if p == expand(q) {
			return true
		}
	}
	return false
}

// raw mode terminal helpers using darwin ioctl (TIOCGETA / TIOCSETA).
func makeRaw(fd int) (*syscall.Termios, error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGETA, uintptr(unsafe.Pointer(&old)), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(&raw)), 0, 0, 0); errno != 0 {
		return nil, errno
	}
	return &old, nil
}

func restoreTerm(fd int, old *syscall.Termios) {
	if old != nil {
		_, _, _ = syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(old)), 0, 0, 0)
	}
}

func termSize(fd int) (rows, cols int) {
	var ws struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)), 0, 0, 0); errno == 0 && ws.Row > 0 {
		return int(ws.Row), int(ws.Col)
	}
	return 24, 100
}

// selectState holds the UI state of the interactive checklist.
type selectState struct {
	items   []planItem
	checked []bool
	cursor  int
	offset  int
}

func newSelectState(items []planItem) *selectState {
	return &selectState{items: items, checked: make([]bool, len(items))}
}

// handleKey processes one input packet in raw mode.
// Returns (proceed, cancel).
func (st *selectState) handleKey(buf []byte) (proceed, cancel bool) {
	n := len(buf)
	if n == 0 {
		return false, true
	}
	switch {
	case buf[0] == 3 || buf[0] == 'q' || buf[0] == 'Q' || (n == 1 && buf[0] == 27): // Ctrl-C, q, Esc
		return false, true
	case buf[0] == ' ': // Space toggles current item
		st.checked[st.cursor] = !st.checked[st.cursor]
	case buf[0] == '\r' || buf[0] == '\n': // Enter proceeds to confirmation
		return true, false
	case buf[0] == 'j' || (n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == 'B'):
		if st.cursor+1 < len(st.items) {
			st.cursor++
		}
	case buf[0] == 'k' || (n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == 'A'):
		if st.cursor > 0 {
			st.cursor--
		}
	case n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == '6': // Page Down
		st.cursor = min(len(st.items)-1, st.cursor+15)
	case n >= 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == '5': // Page Up
		st.cursor = max(0, st.cursor-15)
	case buf[0] == 'a': // toggle all
		anyUnchecked := false
		for _, c := range st.checked {
			if !c {
				anyUnchecked = true
				break
			}
		}
		for i := range st.checked {
			st.checked[i] = anyUnchecked
		}
	case buf[0] == 'g': // toggle all 🟢 regenerable
		anyUnchecked := false
		for i, it := range st.items {
			if it.rule.Safety == Regenerable && !st.checked[i] {
				anyUnchecked = true
				break
			}
		}
		for i, it := range st.items {
			if it.rule.Safety == Regenerable {
				st.checked[i] = anyUnchecked
			}
		}
	}
	return false, false
}

func (st *selectState) renderTo(w io.Writer, rows, cols int) {
	pageSize := rows - 7
	if pageSize < 5 {
		pageSize = 5
	}
	if st.cursor < st.offset {
		st.offset = st.cursor
	} else if st.cursor >= st.offset+pageSize {
		st.offset = st.cursor - pageSize + 1
	}

	var selCount int
	var selBytes int64
	for i, c := range st.checked {
		if c {
			selCount++
			selBytes += st.items[i].size
		}
	}

	var b strings.Builder
	b.WriteString("\033[H\033[2J")
	fmt.Fprintf(&b, "\033[1mdiskwise select\033[0m · %d recognised items · \033[1m%d checked (%s)\033[0m\r\n",
		len(st.items), selCount, human(selBytes))
	b.WriteString("  \033[2m↑/↓ or j/k: move · SPACE: toggle check · a: toggle all · g: toggle safe (🟢) · ENTER: proceed · q: quit\033[0m\r\n\r\n")

	end := st.offset + pageSize
	if end > len(st.items) {
		end = len(st.items)
	}
	pathWidth := cols - 44
	if pathWidth < 25 {
		pathWidth = 25
	}
	for i := st.offset; i < end; i++ {
		it := st.items[i]
		ptr := "  "
		if i == st.cursor {
			ptr = "\033[1;36m▸ \033[0m"
		}
		box := "[ ]"
		rowStart, rowEnd := "", ""
		if st.checked[i] {
			box = "\033[1;32m[✓]\033[0m"
			rowStart = "\033[1m"
			rowEnd = "\033[0m"
		}
		desc := it.what()
		if it.rule.Action == None {
			desc = "delete " + display(it.path)
		}
		fmt.Fprintf(&b, "%s%s %s %s%8s  %4s  %-16s  %s%s\r\n",
			ptr, box, it.rule.Safety.Icon(), rowStart,
			human(it.size), ago(it.newest), truncMid(it.rule.ID, 16),
			truncMid(desc, pathWidth), rowEnd)
	}

	if len(st.items) > pageSize {
		fmt.Fprintf(&b, "\r\n  \033[2mshowing %d–%d of %d\033[0m", st.offset+1, end, len(st.items))
	}
	cur := st.items[st.cursor]
	if cur.rule.Note != "" {
		fmt.Fprintf(&b, "\r\n  \033[2m%s: %s\033[0m", cur.rule.Name, cur.rule.Note)
	}
	_, _ = io.WriteString(w, b.String())
}

func (st *selectState) selectedItems() ([]planItem, int64) {
	var selected []planItem
	var total int64
	for i, it := range st.items {
		if st.checked[i] {
			selected = append(selected, it)
			total += it.size
		}
	}
	return selected, total
}

func runSelect(s *Scan, o selectOptions) error {
	items := buildSelectList(s, o)
	if len(items) == 0 {
		fmt.Println("no matching recognised items found (try --min-size 1M or --all, or run `diskwise scan`)")
		return nil
	}
	if !isTTY(os.Stdin) || !isTTY(os.Stdout) {
		return fmt.Errorf("interactive selector requires a terminal (TTY)")
	}

	st := newSelectState(items)

	oldTerm, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("cannot enter raw terminal mode: %w", err)
	}
	// Alternate screen + hide cursor for clean UI
	fmt.Print("\033[?1049h\033[?25l")
	cleanupTTY := func() {
		fmt.Print("\033[?25h\033[?1049l")
		restoreTerm(int(os.Stdin.Fd()), oldTerm)
	}

	buf := make([]byte, 16)
	for {
		rows, cols := termSize(int(os.Stdout.Fd()))
		st.renderTo(os.Stdout, rows, cols)
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			cleanupTTY()
			return nil
		}
		proceed, cancel := st.handleKey(buf[:n])
		if cancel {
			cleanupTTY()
			fmt.Println("cancelled — nothing was deleted.")
			return nil
		}
		if proceed {
			break
		}
	}
	cleanupTTY()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	selected, total := st.selectedItems()
	return confirmAndExecute(selected, total, o.countdown, time.Second, os.Stdin, os.Stdout, sigCh)
}

// confirmAndExecute runs Warning 1 ("Write CONFIRM"), Warning 2 (countdown with
// abort channel), and executes the selected cleanup actions.
func confirmAndExecute(selected []planItem, total int64, secs int, tick time.Duration, in io.Reader, out io.Writer, abort <-chan os.Signal) error {
	if len(selected) == 0 {
		fmt.Fprintln(out, "no items checked — nothing to delete.")
		return nil
	}

	fmt.Fprintf(out, "\n\033[1mSELECTED FOR DELETION\033[0m (%d items, up to %s)\n", len(selected), human(total))
	for _, it := range selected {
		desc := it.what()
		if it.rule.Action == None {
			desc = "delete " + display(it.path)
		}
		fmt.Fprintf(out, "  [✓] %s %8s  %-16s %s\n", it.rule.Safety.Icon(), human(it.size), it.rule.ID, desc)
	}

	// Warning 1: explicit CONFIRM prompt
	fmt.Fprintf(out, "\n\033[1;33m⚠ WARNING 1/2:\033[0m The %d checked items above (%s) will be permanently deleted.\n",
		len(selected), human(total))
	fmt.Fprint(out, "Please confirm you want this: Write CONFIRM: ")
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		fmt.Fprintln(out, "Aborted (did not write CONFIRM). Nothing was deleted.")
		return nil
	}
	if strings.TrimSpace(line) != "CONFIRM" {
		fmt.Fprintln(out, "Aborted (did not write CONFIRM). Nothing was deleted.")
		return nil
	}

	// Warning 2: countdown with Ctrl-C abort
	if secs <= 0 {
		secs = 5
	}
	fmt.Fprintf(out, "\n\033[1;31m⚠ WARNING 2/2:\033[0m Final countdown — press Ctrl-C now to abort!\n")
	for left := secs; left > 0; left-- {
		fmt.Fprintf(out, "\r  Deleting in %d second(s)... (Ctrl-C to cancel) ", left)
		select {
		case <-abort:
			fmt.Fprintln(out, "\n\nAborted during countdown. Nothing was deleted.")
			return nil
		case <-time.After(tick):
		}
	}
	fmt.Fprintf(out, "\r\033[K  Starting cleanup...\n\n")

	m := newMatcher()
	before := freeBytes(homeDir)
	for _, it := range selected {
		// If the user explicitly opted into a review-only rule with --all, delete its directory.
		execItem := it
		if execItem.rule.Action == None {
			rCopy := *execItem.rule
			rCopy.Action = Delete
			execItem.rule = &rCopy
		}
		if err := execute(execItem, m); err != nil {
			fmt.Fprintf(out, "  ✗ %v\n", err)
			logAction(it, err)
		} else {
			fmt.Fprintf(out, "  ✓ %s (%s)\n", execItem.what(), human(it.size))
			logAction(it, nil)
		}
	}
	freed := freeBytes(homeDir) - before
	fmt.Fprintf(out, "\nDone. Free space changed by %s (APFS may take a moment to reclaim; log: %s)\n",
		human(max(freed, 0)), display(actionLogPath()))
	return nil
}
