// diskwise finds what fills a macOS disk and frees space safely.
//
//	diskwise scan      measure the Data volume (no LLM)
//	diskwise report    biggest buckets + recognised rule matches
//	diskwise explain   ask an LLM about buckets no rule recognises
//	diskwise select    interactively check items with SPACE, then CONFIRM + countdown
//	diskwise clean     dry run (or --apply) of safe cleanups
//	diskwise rules     list the built-in rule catalog
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

const usage = `diskwise — find what fills your disk and free space safely

usage:
  diskwise scan    [--root PATH] [--keep-min 50M] [--exclude PATH]...
  diskwise report  [--min 10G] [--limit 30] [--rule ID] [--json]
  diskwise explain [--unknown] [--fresh] [--dry] [PATH...]
  diskwise select  [--all] [--rule ID,...] [--older-than 30d] [--min-size 10M] [--countdown 5]
  diskwise clean   [-i] [--costly] [--rule ID,...] [--older-than 30d] [--min-size 100M] [--apply [--yes]]
  diskwise rules

global: --scan-file PATH   (default ~/.cache/diskwise/scan.json)

LLM for explain (first configured wins, or force with DISKWISE_LLM=cmd|anthropic|openai|gemini|claude):
  DISKWISE_LLM_CMD   shell command that reads the prompt on stdin
  ANTHROPIC_API_KEY  · OPENAI_API_KEY (+OPENAI_BASE_URL, e.g. http://localhost:11434/v1 for Ollama)
  GEMINI_API_KEY     · the claude CLI on PATH
  DISKWISE_MODEL     overrides the model name
`

func main() {
	args := os.Args[1:]
	scanFile := defaultScanFile()
	// pull out the global flag wherever it appears
	for i := 0; i < len(args); i++ {
		if args[i] == "--scan-file" && i+1 < len(args) {
			scanFile = expand(args[i+1])
			args = append(args[:i], args[i+2:]...)
			i--
		} else if strings.HasPrefix(args[i], "--scan-file=") {
			scanFile = expand(strings.TrimPrefix(args[i], "--scan-file="))
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}
	if len(args) == 0 {
		if s, err := loadScan(scanFile); err == nil {
			printReport(os.Stdout, s, reportOptions{})
			return
		}
		fmt.Print(usage)
		return
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "scan":
		cmdScan(rest, scanFile)
	case "report":
		cmdReport(rest, scanFile)
	case "explain":
		cmdExplain(rest, scanFile)
	case "select", "tui":
		cmdSelect(rest, scanFile)
	case "clean":
		cmdClean(rest, scanFile)
	case "rules":
		printRules(os.Stdout)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func sizeFlag(fs *flag.FlagSet, name, def, help string) *string { return fs.String(name, def, help) }

func mustSize(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := parseSize(s)
	if err != nil {
		fatalf("%v", err)
	}
	return v
}

func cmdScan(args []string, scanFile string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	root := fs.String("root", defaultRoot(), "directory to scan (default: the whole Data volume)")
	keep := sizeFlag(fs, "keep-min", "50M", "keep directories at least this large in the saved tree")
	workers := fs.Int("workers", runtime.NumCPU()*4, "parallel directory readers")
	quiet := fs.Bool("quiet", false, "no progress output")
	noReport := fs.Bool("no-report", false, "don't print the report after scanning")
	var excludes multiFlag
	fs.Var(&excludes, "exclude", "skip this directory (repeatable)")
	_ = fs.Parse(args)

	s, err := runScan(scanOptions{root: *root, keepMin: mustSize(*keep), excludes: excludes, workers: *workers,
		progress: !*quiet && isTTY(os.Stderr)})
	if err != nil {
		fatalf("%v", err)
	}
	if err := saveScan(s, scanFile); err != nil {
		fatalf("saving scan: %v", err)
	}
	fmt.Fprintf(os.Stderr, "scanned %s in %s files (%s unreadable) in %.0fs → %s\n", human(s.Tree.Size),
		commas(s.Tree.Files), commas(s.Errors), s.Seconds, display(scanFile))
	if !*noReport {
		printReport(os.Stdout, s, reportOptions{})
	}
}

func cmdReport(args []string, scanFile string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	min := sizeFlag(fs, "min", "", "bucket threshold (default: 1% of used space, at least 1G)")
	limit := fs.Int("limit", 30, "max buckets to show")
	rule := fs.String("rule", "", "list every match of one rule")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	s, err := loadScan(scanFile)
	if err != nil {
		fatalf("%v", err)
	}
	printReport(os.Stdout, s, reportOptions{min: mustSize(*min), rule: *rule, asJSON: *asJSON, limit: *limit})
}

func cmdExplain(args []string, scanFile string) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	unknown := fs.Bool("unknown", false, "explain every unknown bucket of the last scan")
	fresh := fs.Bool("fresh", false, "ignore cached answers and known rules")
	dry := fs.Bool("dry", false, "print the prompt that would be sent, don't call the LLM")
	min := sizeFlag(fs, "min", "", "bucket threshold for --unknown")
	_ = fs.Parse(args)
	if err := runExplain(explainOptions{paths: fs.Args(), unknown: *unknown, fresh: *fresh, dry: *dry,
		min: mustSize(*min)}, scanFile); err != nil {
		fatalf("%v", err)
	}
}

func cmdSelect(args []string, scanFile string) {
	fs := flag.NewFlagSet("select", flag.ExitOnError)
	all := fs.Bool("all", false, "include 🔴 user-data rules too (still excludes ⛔ system & protected folders)")
	rules := fs.String("rule", "", "only these rule ids (comma-separated)")
	older := fs.String("older-than", "", "only matches untouched for this long, e.g. 30d")
	minSize := sizeFlag(fs, "min-size", "10M", "skip matches smaller than this")
	countdown := fs.Int("countdown", 5, "seconds to count down after writing CONFIRM")
	_ = fs.Parse(args)

	o := selectOptions{allRules: *all, minSize: mustSize(*minSize), countdown: *countdown, rules: map[string]bool{}}
	for _, id := range strings.Split(*rules, ",") {
		if id = strings.TrimSpace(id); id != "" {
			if ruleByID[id] == nil {
				fatalf("unknown rule %q; see `diskwise rules`", id)
			}
			o.rules[id] = true
		}
	}
	var err error
	if o.olderThan, err = parseAge(*older); err != nil {
		fatalf("%v", err)
	}
	s, err := loadScan(scanFile)
	if err != nil {
		fatalf("%v", err)
	}
	if err := runSelect(s, o); err != nil {
		fatalf("%v", err)
	}
}

func cmdClean(args []string, scanFile string) {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	interactive := fs.Bool("i", false, "interactive selector (same as `diskwise select`)")
	costly := fs.Bool("costly", false, "include 🟡 items (re-downloadable, slow to rebuild)")
	rules := fs.String("rule", "", "only these rule ids (comma-separated); naming a 🟡 rule includes it")
	older := fs.String("older-than", "", "only matches untouched for this long, e.g. 30d")
	minSize := sizeFlag(fs, "min-size", "1M", "skip matches smaller than this")
	apply := fs.Bool("apply", false, "actually do it (asks per item)")
	yes := fs.Bool("yes", false, "with --apply: don't ask")
	_ = fs.Parse(args)

	if *interactive {
		var filtered []string
		for _, a := range args {
			if a != "-i" && a != "--i" {
				filtered = append(filtered, a)
			}
		}
		cmdSelect(filtered, scanFile)
		return
	}

	o := cleanOptions{costly: *costly, minSize: mustSize(*minSize), apply: *apply, yes: *yes, rules: map[string]bool{}}
	for _, id := range strings.Split(*rules, ",") {
		if id = strings.TrimSpace(id); id != "" {
			if ruleByID[id] == nil {
				fatalf("unknown rule %q; see `diskwise rules`", id)
			}
			o.rules[id] = true
		}
	}
	var err error
	if o.olderThan, err = parseAge(*older); err != nil {
		fatalf("%v", err)
	}
	s, err := loadScan(scanFile)
	if err != nil {
		fatalf("%v", err)
	}
	if err := runClean(s, o); err != nil {
		fatalf("%v", err)
	}
}
