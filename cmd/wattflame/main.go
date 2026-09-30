// wattflame shows which functions of a program use the most energy.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/useless-husband/wattflame/internal/profile"
	"github.com/useless-husband/wattflame/internal/report"
)

var version = "dev"

const usage = `wattflame - an energy profiler for macOS on Apple Silicon

It samples a program's call stacks, reads the energy the kernel billed to each
thread, and shows which functions the energy went to.

Usage:
  wattflame record [flags] -- <command> [args...]   run a program and profile it
  wattflame record [flags] -p <pid>                 profile a running process (needs sudo)
  wattflame report [flags] <profile.json>           summarise or convert a saved profile
  wattflame diff   [flags] <before.json> <after.json>
  wattflame version

Run "wattflame <command> -h" for the flags of a command.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "record":
		return cmdRecord(args[1:], stdout, stderr)
	case "report":
		return cmdReport(args[1:], stdout, stderr)
	case "diff":
		return cmdDiff(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "wattflame %s\n", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "wattflame: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: wattflame %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// parse runs fs.Parse and maps its outcome to an exit code (-1 = carry on).
func parse(fs *flag.FlagSet, args []string) int {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return -1
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "wattflame: %v\n", err)
	return 1
}

func cmdReport(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("report", "report [flags] <profile.json>", stderr)
	htmlOut := fs.String("html", "", "write the flame graph page to this `file`")
	folded := fs.String("folded", "", "write folded stacks (flamegraph.pl / speedscope format) to this `file`, or - for stdout")
	metric := fs.String("metric", "energy", "weight of the folded stacks: energy (µJ), cpu (µs) or samples")
	threads := fs.Bool("threads", false, "keep a thread level in the folded stacks")
	top := fs.Int("top", 15, "number of functions in the summary")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	p, err := profile.Load(fs.Arg(0))
	if err != nil {
		return fail(stderr, err)
	}

	if *folded != "" {
		m, err := profile.ParseMetric(*metric)
		if err != nil {
			return fail(stderr, err)
		}
		out := stdout
		if *folded != "-" {
			f, err := os.Create(*folded)
			if err != nil {
				return fail(stderr, err)
			}
			defer f.Close()
			out = f
		}
		if err := p.WriteFolded(out, m, *threads); err != nil {
			return fail(stderr, err)
		}
		if *folded == "-" {
			return 0
		}
	}
	if *htmlOut != "" {
		if err := report.SaveHTML(*htmlOut, p); err != nil {
			return fail(stderr, err)
		}
	}
	if err := report.Summary(stdout, p, *top); err != nil {
		return fail(stderr, err)
	}
	if *htmlOut != "" {
		fmt.Fprintf(stdout, "  flame graph  %s\n\n", *htmlOut)
	}
	return 0
}

func cmdDiff(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("diff", "diff [flags] <before.json> <after.json>", stderr)
	top := fs.Int("top", 20, "number of functions to list")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	before, err := profile.Load(fs.Arg(0))
	if err != nil {
		return fail(stderr, err)
	}
	after, err := profile.Load(fs.Arg(1))
	if err != nil {
		return fail(stderr, err)
	}
	if err := report.Diff(stdout, before, after, *top); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func htmlPathFor(output string) string {
	return strings.TrimSuffix(output, ".json") + ".html"
}
