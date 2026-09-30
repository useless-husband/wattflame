package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/useless-husband/wattflame/internal/profile"
)

// Title is a one-line description of what was profiled.
func Title(p *profile.Profile) string {
	if len(p.Meta.Command) > 0 {
		return strings.Join(p.Meta.Command, " ")
	}
	if len(p.Meta.Processes) > 0 {
		return fmt.Sprintf("%s (pid %d)", p.Meta.Processes[0].Name, p.Meta.PID)
	}
	return fmt.Sprintf("pid %d", p.Meta.PID)
}

// Coverage returns the kernel's per-process energy totals for the tracked
// processes, and how much of that the per-thread counters showed directly.
// The profile's own total can be set against kernel; what the threads did not
// show was taken from the process totals. kernel is 0 when the recording did
// not capture it.
func Coverage(p *profile.Profile) (threads, kernel uint64) {
	for _, pr := range p.Meta.Processes {
		kernel += pr.KernelEnergyNJ
		threads += pr.ThreadsEnergyNJ
	}
	return threads, kernel
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := (max - 1) * 2 / 3
	tail := max - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// Summary writes the terminal report: headline numbers and the functions that
// used the most energy.
func Summary(w io.Writer, p *profile.Profile, top int) error {
	total := p.Total()
	energy := total.Energy()
	cpu := total.CPU()

	fmt.Fprintf(w, "\n  %s\n", truncate(Title(p), 100))
	sub := []string{}
	if p.Meta.PID != 0 {
		sub = append(sub, fmt.Sprintf("pid %d", p.Meta.PID))
	}
	if p.Meta.Exit != "" {
		sub = append(sub, p.Meta.Exit)
	}
	if p.Meta.Machine.Chip != "" {
		sub = append(sub, p.Meta.Machine.Chip)
	}
	if len(sub) > 0 {
		fmt.Fprintf(w, "  %s\n", strings.Join(sub, " · "))
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	avg := 0.0
	if p.Meta.DurationNs > 0 {
		avg = float64(energy) / float64(p.Meta.DurationNs)
	}
	fmt.Fprintf(tw, "  Energy\t%s\t%s average over %s\n", Energy(energy), Watts(avg), Duration(p.Meta.DurationNs))
	fmt.Fprintf(tw, "  CPU time\t%s\t%s while on a core\n", Duration(cpu), Watts(total.Watts()))
	tw.Flush()

	if len(p.Levels) > 1 {
		parts := make([]string, 0, len(p.Levels))
		for l, lv := range p.Levels {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", lv.Name, Energy(total.EnergyNJ[l]), Percent(total.EnergyNJ[l], energy)))
		}
		fmt.Fprintf(w, "  Cores      %s\n", strings.Join(parts, " · "))
	}
	if th, k := Coverage(p); k > 0 {
		what := "the process"
		if len(p.Meta.Processes) > 1 {
			what = fmt.Sprintf("the %d processes it tracked", len(p.Meta.Processes))
		}
		line := fmt.Sprintf("  Accounted  %s of what the kernel billed to %s", Percent(energy, k), what)
		if th < k*99/100 {
			// Threads that end between two readings take their last
			// moments with them; that part comes from the process total.
			line += fmt.Sprintf(" (%s read thread by thread)", Percent(th, k))
		}
		fmt.Fprintln(w, line)
	}
	if tree := p.Meta.TreeCPUNs; tree > 0 && (len(p.Meta.Processes) > 1 || cpu < tree*99/100) {
		// Processes too short-lived to be noticed, and ones owned by another
		// user, are in the kernel's figure for the whole tree but not here.
		fmt.Fprintf(w, "  Seen       %s of the %s of CPU time the command and its children used\n", Percent(cpu, tree), Duration(tree))
	}
	if s := p.Meta.Sampler; s.Ticks > 0 {
		hz := 0
		if p.Meta.IntervalUs > 0 {
			hz = 1000000 / p.Meta.IntervalUs
		}
		line := fmt.Sprintf("  Profiler   %s of its own (%s on top), %d Hz", Energy(s.SelfEnergyNJ), Percent(s.SelfEnergyNJ, energy), hz)
		if s.Samples > 0 {
			line += fmt.Sprintf(", %s pause per stack", Duration(s.SuspendNs/s.Samples))
		}
		if s.Overruns > 0 {
			line += fmt.Sprintf(", %d of %d ticks late", s.Overruns, s.Ticks+s.Overruns)
		}
		if s.Dropped > 0 {
			line += fmt.Sprintf(", %d records dropped", s.Dropped)
		}
		if s.TargetsLost > 0 {
			line += fmt.Sprintf(", %d processes not tracked (out of memory)", s.TargetsLost)
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)

	if energy == 0 {
		fmt.Fprintln(w, "  No energy was recorded. The program may have exited before the first sample,")
		fmt.Fprintln(w, "  or the kernel on this machine does not report per-thread energy.")
		fmt.Fprintln(w)
		return nil
	}

	all := p.Functions()
	funcs := all
	if top > 0 && len(funcs) > top {
		funcs = funcs[:top]
	}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  SELF\t\tTOTAL\tPOWER\t  FUNCTION\n")
	shown := 0
	for _, f := range funcs {
		if f.Self.Energy() == 0 && f.Total.Energy() == 0 {
			continue
		}
		fr := &p.Frames[f.Frame]
		name := truncate(fr.Name, 70)
		if fr.Module != "" && !strings.HasPrefix(fr.Name, "[") {
			name += "  " + fr.Module
		}
		power := "–"
		if f.Self.CPU() > 0 {
			power = Watts(f.Self.Watts())
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t  %s\n",
			Energy(f.Self.Energy()), Percent(f.Self.Energy(), energy),
			Percent(f.Total.Energy(), energy), power, name)
		shown++
	}
	tw.Flush()
	if rest := len(all) - shown; rest == 1 {
		fmt.Fprintln(w, "  … and 1 more function")
	} else if rest > 1 {
		fmt.Fprintf(w, "  … and %d more functions\n", rest)
	}
	fmt.Fprintln(w)
	return nil
}
