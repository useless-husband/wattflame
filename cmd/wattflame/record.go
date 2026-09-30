package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/useless-husband/wattflame/internal/preloadlib"
	"github.com/useless-husband/wattflame/internal/profile"
	"github.com/useless-husband/wattflame/internal/report"
	"github.com/useless-husband/wattflame/internal/sampler"
)

func cmdRecord(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record", "record [flags] -- <command> [args...]\n       wattflame record [flags] -p <pid>", stderr)
	var output, htmlOut string
	var pid, hz, depth, top int
	var duration time.Duration
	var noHTML, open, quiet bool
	fs.StringVar(&output, "o", "wattflame.json", "write the profile to this `file`")
	fs.StringVar(&htmlOut, "html", "", "write the flame graph page to this `file` (default: the profile name with .html)")
	fs.BoolVar(&noHTML, "no-html", false, "do not write a flame graph page")
	fs.IntVar(&pid, "p", 0, "attach to this running `pid` instead of launching a command (needs sudo)")
	fs.IntVar(&hz, "hz", 1000, "stack samples per second")
	fs.DurationVar(&duration, "d", 0, "stop after this `duration`, for example 10s (default: until the program exits)")
	fs.IntVar(&depth, "depth", 256, "deepest stack to record, in frames")
	fs.IntVar(&top, "top", 15, "number of functions in the summary")
	fs.BoolVar(&open, "open", false, "open the flame graph in the browser when done")
	fs.BoolVar(&quiet, "q", false, "print nothing but errors")
	if code := parse(fs, args); code >= 0 {
		return code
	}
	command := fs.Args()

	if err := sampler.Supported(); err != nil {
		return fail(stderr, err)
	}
	if hz < 1 || hz > 10000 {
		return fail(stderr, errors.New("-hz must be between 1 and 10000"))
	}
	if (pid != 0) == (len(command) > 0) {
		fmt.Fprintln(stderr, "wattflame: give either a command to run or -p <pid>")
		fs.Usage()
		return 2
	}
	if htmlOut == "" && !noHTML {
		htmlOut = htmlPathFor(output)
	}

	// Signals are caught before anything is started: from here on wattflame
	// must not die in a way that leaves the program it launched behind.
	// SIGQUIT is included because Go's default for it is to exit on the spot.
	sigc := make(chan os.Signal, 8)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigc)

	var sess *sampler.Session
	var err error
	launched := "" // resolved path of the program, in launch mode
	if pid != 0 {
		sess, err = sampler.Attach(pid)
	} else {
		var preload string
		preload, err = preloadlib.Path()
		if err != nil {
			return fail(stderr, err)
		}
		// Resolve the program ourselves so "not found" is reported before
		// anything is set up.
		path, lookErr := exec.LookPath(command[0])
		if lookErr != nil {
			return fail(stderr, fmt.Errorf("cannot run %s: %w", command[0], lookErr))
		}
		launched = path
		argv := append([]string{path}, command[1:]...)
		sess, err = sampler.Launch(argv, os.Environ(), preload)
	}
	if err != nil {
		return fail(stderr, err)
	}
	defer sess.Close()

	b := profile.NewBuilder(sess)
	var targets []sampler.TargetInfo
	syncTargets := func() {
		ts := sess.Targets()
		for i := len(targets); i < len(ts); i++ {
			b.SetProcess(uint32(i), ts[i].Name, ts[i].Path)
		}
		targets = ts
	}
	syncTargets()

	started := time.Now()
	interval := time.Second / time.Duration(hz)
	if err := sess.Start(interval, depth); err != nil {
		return fail(stderr, err)
	}
	if pid != 0 && !quiet {
		if duration > 0 {
			fmt.Fprintf(stderr, "wattflame: recording pid %d for %s (Ctrl-C to stop early)\n", pid, duration)
		} else {
			fmt.Fprintf(stderr, "wattflame: recording pid %d (Ctrl-C to stop)\n", pid)
		}
	}

	var deadline <-chan time.Time
	if duration > 0 {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	stopped := "" // why recording ended before the program did
	interrupts := 0
loop:
	for {
		select {
		case <-ticker.C:
			syncTargets()
			sess.Drain(b.Add)
			sess.Reap()
			if exited, _ := sess.RootExited(); exited {
				break loop
			}
		case sig := <-sigc:
			interrupts++
			if pid != 0 || interrupts > 1 {
				stopped = "recording interrupted"
				break loop
			}
			// Ctrl-C at a terminal goes to the whole foreground process
			// group, so the program has had it already. Anything else was
			// sent to wattflame alone; pass it on. Either way keep recording
			// until the program exits.
			if sig != syscall.SIGINT || !inForeground() {
				sess.Signal(sig)
			}
			if !quiet {
				fmt.Fprintln(stderr, "wattflame: waiting for the program to exit (Ctrl-C again to stop now)")
			}
		case <-deadline:
			stopped = fmt.Sprintf("stopped after %s", duration)
			break loop
		}
	}

	sess.Stop()
	syncTargets()
	sess.Drain(b.Add)
	syncTargets()

	exited, status := sess.RootExited()
	stats := sess.Stats()
	meta := profile.Meta{
		Tool:       "wattflame " + version,
		Command:    command,
		PID:        sess.RootPID(),
		TreeCPUNs:  uint64(sess.RootTreeCPU()),
		Attached:   pid != 0,
		Started:    started,
		DurationNs: stats.ElapsedNs,
		IntervalUs: int(interval / time.Microsecond),
		Machine:    machineInfo(),
		Sampler: profile.SamplerStats{
			Ticks:        stats.Ticks,
			Overruns:     stats.Overruns,
			Samples:      stats.Samples,
			SampleErrors: stats.SampleErrors,
			SuspendNs:    stats.SuspendNs,
			SuspendMaxNs: stats.SuspendMaxNs,
			SelfEnergyNJ: stats.SelfEnergyNJ,
			SelfCPUNs:    stats.SelfCPUNs,
			Dropped:      stats.Dropped,
			TargetsLost:  stats.TargetsLost,
		},
	}
	exitCode := 0
	switch {
	case exited && pid == 0 && status.Signal != "":
		meta.Exit = "killed by " + status.Signal
		exitCode = 128 + status.SignalNum
	case exited && pid == 0:
		meta.Exit = fmt.Sprintf("exit %d", status.Code)
		exitCode = status.Code
	case exited:
		meta.Exit = "process exited"
	default:
		meta.Exit = stopped
	}
	for _, t := range targets {
		meta.Processes = append(meta.Processes, profile.Process{
			PID: t.PID, Name: t.Name, KernelEnergyNJ: t.EnergyNJ, ThreadsEnergyNJ: t.ThreadsEnergyNJ,
		})
	}

	// A launched program that never became samplable still has its energy
	// recorded, per process. Say why there are no functions to look at.
	if launched != "" && !quiet {
		sampled := false
		for _, t := range targets {
			if t.PID == sess.RootPID() && !t.Opaque {
				sampled = true
			}
		}
		if !sampled {
			fmt.Fprintf(stderr, "wattflame: %s. Its energy was recorded per process, without stacks.\n", whyNoStacks(launched))
		}
	}

	p := b.Finish(meta, perfLevels())
	if err := p.Save(output); err != nil {
		return fail(stderr, err)
	}
	if htmlOut != "" {
		if err := report.SaveHTML(htmlOut, p); err != nil {
			return fail(stderr, err)
		}
	}
	if !quiet {
		if err := report.Summary(stdout, p, top); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "  profile      %s\n", output)
		if htmlOut != "" {
			fmt.Fprintf(stdout, "  flame graph  %s\n", htmlOut)
		}
		fmt.Fprintln(stdout)
	}
	if open && htmlOut != "" {
		if err := exec.Command("open", htmlOut).Start(); err != nil {
			fmt.Fprintf(stderr, "wattflame: cannot open %s: %v\n", htmlOut, err)
		}
	}
	return exitCode
}

func perfLevels() []profile.Level {
	names := sampler.PerfLevels()
	levels := make([]profile.Level, len(names))
	for i, n := range names {
		levels[i] = profile.Level{Name: n, Cores: levelCores(i)}
	}
	return levels
}
