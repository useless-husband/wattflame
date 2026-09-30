// Package report renders a profile for people: a terminal summary, a
// self-contained HTML flame graph, and a before/after comparison.
package report

import (
	"fmt"
	"time"
)

// Energy formats nanojoules with a unit that keeps three significant digits.
func Energy(nj uint64) string {
	v := float64(nj)
	switch {
	case nj == 0:
		return "0 J"
	case v < 1e3:
		return fmt.Sprintf("%d nJ", nj)
	case v < 1e6:
		return sig3(v/1e3) + " µJ"
	case v < 1e9:
		return sig3(v/1e6) + " mJ"
	case v < 1e12:
		return sig3(v/1e9) + " J"
	}
	return sig3(v/1e12) + " kJ"
}

// Duration formats nanoseconds the same way.
func Duration(ns uint64) string {
	v := float64(ns)
	switch {
	case ns == 0:
		return "0 s"
	case v < 1e3:
		return fmt.Sprintf("%d ns", ns)
	case v < 1e6:
		return sig3(v/1e3) + " µs"
	case v < 1e9:
		return sig3(v/1e6) + " ms"
	case v < 600e9:
		return sig3(v/1e9) + " s"
	}
	return time.Duration(ns).Round(time.Second).String()
}

// Watts formats a power value.
func Watts(w float64) string {
	switch {
	case w == 0:
		return "0 W"
	case w < 0.001:
		return sig3(w*1e6) + " µW"
	case w < 1:
		return sig3(w*1e3) + " mW"
	}
	return sig3(w) + " W"
}

// Percent formats part/whole.
func Percent(part, whole uint64) string {
	if whole == 0 {
		return "–"
	}
	p := 100 * float64(part) / float64(whole)
	switch {
	case part == 0:
		return "0%"
	case p < 0.1:
		return "<0.1%"
	case p >= 99.995 && part < whole:
		return ">99.99%"
	case p >= 99.95 && p < 100.05 && part != whole:
		return fmt.Sprintf("%.2f%%", p)
	}
	return fmt.Sprintf("%.1f%%", p)
}

// sig3 prints v (expected in [1, 1000)) with three significant digits.
func sig3(v float64) string {
	switch {
	case v >= 99.95:
		return fmt.Sprintf("%.0f", v)
	case v >= 9.995:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}
