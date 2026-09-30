package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/useless-husband/wattflame/internal/profile"
)

// FuncDelta is one function's self energy before and after a change.
type FuncDelta struct {
	Name   string
	Module string
	Before uint64
	After  uint64
}

// Delta is After minus Before in nanojoules.
func (d FuncDelta) Delta() int64 { return int64(d.After) - int64(d.Before) }

type funcKey struct{ module, name string }

func selfByFunc(p *profile.Profile) map[funcKey]uint64 {
	out := make(map[funcKey]uint64)
	for _, f := range p.Functions() {
		fr := &p.Frames[f.Frame]
		out[funcKey{fr.Module, fr.Name}] += f.Self.Energy()
	}
	return out
}

// Compare matches functions by module and name and returns them ordered by
// the size of the change, largest first.
func Compare(before, after *profile.Profile) []FuncDelta {
	b, a := selfByFunc(before), selfByFunc(after)
	keys := make(map[funcKey]struct{}, len(b)+len(a))
	for k := range b {
		keys[k] = struct{}{}
	}
	for k := range a {
		keys[k] = struct{}{}
	}
	out := make([]FuncDelta, 0, len(keys))
	for k := range keys {
		d := FuncDelta{Name: k.name, Module: k.module, Before: b[k], After: a[k]}
		if d.Before == 0 && d.After == 0 {
			continue
		}
		out = append(out, d)
	}
	abs := func(v int64) int64 {
		if v < 0 {
			return -v
		}
		return v
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := abs(out[i].Delta()), abs(out[j].Delta())
		if di != dj {
			return di > dj
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Module < out[j].Module
	})
	return out
}

func signedEnergy(d int64) string {
	switch {
	case d > 0:
		return "+" + Energy(uint64(d))
	case d < 0:
		return "−" + Energy(uint64(-d))
	}
	return "0 J"
}

func change(before, after float64) string {
	if before == 0 {
		if after == 0 {
			return "no change"
		}
		return "new"
	}
	p := 100 * (after - before) / before
	switch {
	case p > 0.05:
		return fmt.Sprintf("+%.1f%%", p)
	case p < -0.05:
		return fmt.Sprintf("−%.1f%%", -p)
	}
	return "no change"
}

// Diff writes a before/after comparison of two profiles.
func Diff(w io.Writer, before, after *profile.Profile, top int) error {
	bt, at := before.Total(), after.Total()
	be, ae := bt.Energy(), at.Energy()

	fmt.Fprintf(w, "\n  before  %s\n", truncate(Title(before), 90))
	fmt.Fprintf(w, "  after   %s\n\n", truncate(Title(after), 90))

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "  \tBEFORE\tAFTER\tCHANGE\n")
	fmt.Fprintf(tw, "  Energy\t%s\t%s\t%s\n", Energy(be), Energy(ae), change(float64(be), float64(ae)))
	fmt.Fprintf(tw, "  CPU time\t%s\t%s\t%s\n", Duration(bt.CPU()), Duration(at.CPU()), change(float64(bt.CPU()), float64(at.CPU())))
	fmt.Fprintf(tw, "  Duration\t%s\t%s\t%s\n", Duration(before.Meta.DurationNs), Duration(after.Meta.DurationNs),
		change(float64(before.Meta.DurationNs), float64(after.Meta.DurationNs)))
	fmt.Fprintf(tw, "  Power on core\t%s\t%s\t%s\n", Watts(bt.Watts()), Watts(at.Watts()), change(bt.Watts(), at.Watts()))
	tw.Flush()
	fmt.Fprintln(w)

	deltas := Compare(before, after)
	if len(deltas) == 0 {
		fmt.Fprintln(w, "  Neither profile contains any energy.")
		fmt.Fprintln(w)
		return nil
	}
	rest := 0
	if top > 0 && len(deltas) > top {
		rest = len(deltas) - top
		deltas = deltas[:top]
	}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "  BEFORE\tAFTER\tCHANGE\t  FUNCTION (self energy)\n")
	for _, d := range deltas {
		name := truncate(d.Name, 70)
		if d.Module != "" && !strings.HasPrefix(d.Name, "[") {
			name += "  " + d.Module
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t  %s\n", Energy(d.Before), Energy(d.After), signedEnergy(d.Delta()), name)
	}
	tw.Flush()
	if rest > 0 {
		fmt.Fprintf(w, "  … and %d more functions\n", rest)
	}
	fmt.Fprintln(w)
	return nil
}
