package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dataPrefix is where the writable APFS Data volume is mounted. Paths under it
// are also reachable without the prefix through firmlinks.
const dataPrefix = "/System/Volumes/Data"

var homeDir = func() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return filepath.Clean(h)
}()

// human formats bytes with decimal units, as Finder and most disk tools do.
func human(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	v := float64(b) / float64(div)
	if v >= 100 {
		return fmt.Sprintf("%.0f %cB", v, "kMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %cB", v, "kMGTPE"[exp])
}

// parseSize parses "500M", "2G", "1.5GB", "1024" (bytes). Decimal units.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(s, "B")
	mult := float64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K':
			mult = 1e3
		case 'M':
			mult = 1e6
		case 'G':
			mult = 1e9
		case 'T':
			mult = 1e12
		}
		if mult != 1 {
			s = s[:len(s)-1]
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(v * mult), nil
}

// parseAge parses "30d", "12h", "2w", "90" (days).
func parseAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	unit := time.Hour * 24
	switch s[len(s)-1] {
	case 'd':
		s = s[:len(s)-1]
	case 'w':
		unit *= 7
		s = s[:len(s)-1]
	case 'h':
		unit = time.Hour
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid age %q (use e.g. 30d)", s)
	}
	return time.Duration(v * float64(unit)), nil
}

// logical maps a real path on the Data volume to the path without the
// Data volume mount prefix.
func logical(p string) string {
	if p == dataPrefix {
		return "/"
	}
	if strings.HasPrefix(p, dataPrefix+"/") {
		return p[len(dataPrefix):]
	}
	return p
}

// display shortens a logical path for humans (home -> ~).
func display(p string) string {
	if p == homeDir {
		return "~"
	}
	if strings.HasPrefix(p, homeDir+"/") {
		return "~" + p[len(homeDir):]
	}
	return p
}

// expand resolves ~ and makes the path absolute and clean.
func expand(p string) string {
	if p == "~" {
		return homeDir
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(homeDir, p[2:])
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return logical(filepath.Clean(p))
}

func ago(unix int64) string {
	if unix <= 0 {
		return "-"
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Hour:
		return "now"
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// truncMid shortens a path to n runes, keeping its start and its end.
func truncMid(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := n / 3
	return string(r[:head]) + "…" + string(r[len(r)-(n-head-1):])
}

func truncLeft(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

func stateDir(kind string) string {
	var d string
	switch kind {
	case "cache":
		d = filepath.Join(homeDir, ".cache", "diskwise")
	default:
		d = filepath.Join(homeDir, ".local", "state", "diskwise")
	}
	_ = os.MkdirAll(d, 0o755)
	return d
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "diskwise: "+format+"\n", a...)
	os.Exit(1)
}
