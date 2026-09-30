// Package profile is wattflame's data model: a call tree whose nodes carry
// energy, CPU time, cycles and instructions, plus the metadata needed to read
// it later on any machine.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// MaxLevels is the largest number of CPU performance levels in a profile.
const MaxLevels = 4

const (
	formatName    = "wattflame-profile"
	formatVersion = 1
)

// Frame kinds. Function frames use the empty string so the common case costs
// nothing in the file.
const (
	KindFunc    = ""
	KindProcess = "process"
	KindThread  = "thread"
)

// Frame origins: whose code a function is.
const (
	OriginApp     = "app"    // the profiled executable itself
	OriginLibrary = "lib"    // a library that is not part of macOS
	OriginSystem  = "system" // shipped with macOS
)

// Weights is what was measured for one tree node: how many stacks landed
// there and what they cost, per performance level (index 0 is the fastest
// cores).
type Weights struct {
	Samples  uint64
	EnergyNJ [MaxLevels]uint64
	CPUNs    [MaxLevels]uint64
	Cycles   [MaxLevels]uint64
	Instr    [MaxLevels]uint64
}

// Add accumulates o into w.
func (w *Weights) Add(o *Weights) {
	w.Samples += o.Samples
	for l := 0; l < MaxLevels; l++ {
		w.EnergyNJ[l] += o.EnergyNJ[l]
		w.CPUNs[l] += o.CPUNs[l]
		w.Cycles[l] += o.Cycles[l]
		w.Instr[l] += o.Instr[l]
	}
}

// IsZero reports whether nothing was measured.
func (w *Weights) IsZero() bool {
	return *w == Weights{}
}

func sum(a [MaxLevels]uint64) uint64 {
	var s uint64
	for _, v := range a {
		s += v
	}
	return s
}

// Energy is the total energy in nanojoules across all performance levels.
func (w *Weights) Energy() uint64 { return sum(w.EnergyNJ) }

// CPU is the total CPU time in nanoseconds.
func (w *Weights) CPU() uint64 { return sum(w.CPUNs) }

// TotalCycles is the CPU cycle count across all levels.
func (w *Weights) TotalCycles() uint64 { return sum(w.Cycles) }

// TotalInstr is the retired instruction count across all levels.
func (w *Weights) TotalInstr() uint64 { return sum(w.Instr) }

// Watts is the average power drawn while this code was on a core. It is what
// separates an energy profile from a time profile: two functions with the same
// CPU time differ here when one runs on efficiency cores or keeps the core at
// a lower power state.
func (w *Weights) Watts() float64 {
	cpu := w.CPU()
	if cpu == 0 {
		return 0
	}
	return float64(w.Energy()) / float64(cpu)
}

// levels returns how many leading levels hold data.
func (w *Weights) levels() int {
	n := 0
	for l := 0; l < MaxLevels; l++ {
		if w.EnergyNJ[l]|w.CPUNs[l]|w.Cycles[l]|w.Instr[l] != 0 {
			n = l + 1
		}
	}
	return n
}

// MarshalJSON writes [samples, L, energy x L, cpu x L, cycles x L, instr x L],
// where L is the number of levels that hold data.
func (w Weights) MarshalJSON() ([]byte, error) {
	n := w.levels()
	out := make([]uint64, 0, 2+4*n)
	out = append(out, w.Samples, uint64(n))
	out = append(out, w.EnergyNJ[:n]...)
	out = append(out, w.CPUNs[:n]...)
	out = append(out, w.Cycles[:n]...)
	out = append(out, w.Instr[:n]...)
	return json.Marshal(out)
}

// UnmarshalJSON reads the array form written by MarshalJSON.
func (w *Weights) UnmarshalJSON(b []byte) error {
	var in []uint64
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*w = Weights{}
	if len(in) == 0 {
		return nil
	}
	if len(in) < 2 {
		return errors.New("weights: too short")
	}
	n := int(in[1])
	if n < 0 || n > MaxLevels || len(in) != 2+4*n {
		return fmt.Errorf("weights: %d values for %d levels", len(in), n)
	}
	w.Samples = in[0]
	copy(w.EnergyNJ[:], in[2:2+n])
	copy(w.CPUNs[:], in[2+n:2+2*n])
	copy(w.Cycles[:], in[2+2*n:2+3*n])
	copy(w.Instr[:], in[2+3*n:2+4*n])
	return nil
}

// Level describes one CPU performance level of the recording machine.
type Level struct {
	Name  string `json:"name"`
	Cores int    `json:"cores,omitempty"`
}

// LineCost is the self energy attributed to one source line of a function.
type LineCost struct {
	Line     uint32 `json:"line"`
	EnergyNJ uint64 `json:"nj"`
}

// Frame is one distinct entry that can appear in the call tree: a function,
// or the synthetic process and thread levels above the stacks.
type Frame struct {
	Kind   string     `json:"kind,omitempty"`
	Name   string     `json:"name"`
	Module string     `json:"module,omitempty"`
	Origin string     `json:"origin,omitempty"`
	File   string     `json:"file,omitempty"`
	PID    int        `json:"pid,omitempty"`
	TID    uint64     `json:"tid,omitempty"`
	Lines  []LineCost `json:"lines,omitempty"`
}

// Node is a call-tree node. Self holds only what was measured with this node
// as the innermost frame; inclusive totals are derived.
type Node struct {
	Parent int32   `json:"p"`
	Frame  int32   `json:"f"`
	Self   Weights `json:"w"`
}

// MarshalJSON omits the weights of nodes that never were a leaf.
func (n Node) MarshalJSON() ([]byte, error) {
	if n.Self.IsZero() {
		return []byte(fmt.Sprintf(`{"p":%d,"f":%d}`, n.Parent, n.Frame)), nil
	}
	w, err := n.Self.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"p":%d,"f":%d,"w":%s}`, n.Parent, n.Frame, w)), nil
}

// Process is one profiled process.
type Process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	// KernelEnergyNJ is the kernel's own per-process total, last read shortly
	// before the recording ended. ThreadsEnergyNJ is what the per-thread
	// counters had added up to at that same moment. Their ratio shows how
	// much of the process's energy the profile can account for.
	KernelEnergyNJ  uint64 `json:"kernelEnergyNJ"`
	ThreadsEnergyNJ uint64 `json:"threadsEnergyNJ"`
}

// Machine identifies the hardware and OS a profile was recorded on.
type Machine struct {
	Model string `json:"model,omitempty"`
	Chip  string `json:"chip,omitempty"`
	OS    string `json:"os,omitempty"`
}

// SamplerStats is the profiler's own cost and health for a recording.
type SamplerStats struct {
	Ticks        uint64 `json:"ticks"`
	Overruns     uint64 `json:"overruns"`
	Samples      uint64 `json:"samples"`
	SampleErrors uint64 `json:"sampleErrors"`
	SuspendNs    uint64 `json:"suspendNs"`
	SuspendMaxNs uint64 `json:"suspendMaxNs"`
	SelfEnergyNJ uint64 `json:"selfEnergyNJ"`
	SelfCPUNs    uint64 `json:"selfCPUNs"`
	Dropped      uint64 `json:"dropped,omitempty"`
	TargetsLost  uint64 `json:"targetsLost,omitempty"`
	// NoCounters is set when the kernel reported no per-thread energy (a
	// virtual machine). The profile then holds stacks and CPU time only.
	NoCounters bool `json:"noCounters,omitempty"`
}

// Meta describes how and where a profile was recorded.
type Meta struct {
	Tool       string    `json:"tool"`
	Command    []string  `json:"command,omitempty"`
	PID        int       `json:"pid"`
	Attached   bool      `json:"attached,omitempty"`
	Started    time.Time `json:"started"`
	DurationNs uint64    `json:"durationNs"`
	IntervalUs int       `json:"intervalUs"`
	Exit       string    `json:"exit,omitempty"`
	// TreeCPUNs is the CPU time the kernel reported, when the launched
	// program was waited for, for it and all the descendants it had reaped.
	// Set against the CPU time in the profile it shows how much of the
	// process tree the recording saw. Zero when unknown.
	TreeCPUNs uint64       `json:"treeCPUNs,omitempty"`
	Machine   Machine      `json:"machine"`
	Processes []Process    `json:"processes"`
	Sampler   SamplerStats `json:"sampler"`
}

// Timeline is energy over time in fixed-width buckets.
type Timeline struct {
	BucketNs uint64     `json:"bucketNs"`
	EnergyNJ [][]uint64 `json:"energyNJ"` // [bucket][level]
}

// Profile is a complete recording.
type Profile struct {
	Format   string   `json:"format"`
	Version  int      `json:"version"`
	Meta     Meta     `json:"meta"`
	Levels   []Level  `json:"levels"`
	Frames   []Frame  `json:"frames"`
	Nodes    []Node   `json:"nodes"` // Nodes[0] is the root; parents precede children
	Timeline Timeline `json:"timeline"`
}

// Validate checks the structural invariants the rest of the code relies on.
func (p *Profile) Validate() error {
	if p.Format != formatName {
		return fmt.Errorf("not a wattflame profile (format %q)", p.Format)
	}
	if p.Version != formatVersion {
		return fmt.Errorf("profile version %d is not supported by this build (wants %d)", p.Version, formatVersion)
	}
	if len(p.Nodes) == 0 {
		return errors.New("profile has no nodes")
	}
	if len(p.Levels) > MaxLevels {
		return fmt.Errorf("profile has %d performance levels, at most %d are supported", len(p.Levels), MaxLevels)
	}
	if p.Nodes[0].Parent != -1 {
		return errors.New("profile root has a parent")
	}
	for i, n := range p.Nodes {
		if i > 0 && (n.Parent < 0 || int(n.Parent) >= i) {
			return fmt.Errorf("node %d has invalid parent %d", i, n.Parent)
		}
		if i > 0 && (n.Frame < 0 || int(n.Frame) >= len(p.Frames)) {
			return fmt.Errorf("node %d has invalid frame %d", i, n.Frame)
		}
	}
	return nil
}

// Write encodes the profile as JSON.
func (p *Profile) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	return enc.Encode(p)
}

// Save writes the profile to a file.
func (p *Profile) Save(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := p.Write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Read decodes and validates a profile.
func Read(r io.Reader) (*Profile, error) {
	var p Profile
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("cannot parse profile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Load reads a profile from a file.
func Load(path string) (*Profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Total sums every node's self weights.
func (p *Profile) Total() Weights {
	var t Weights
	for i := range p.Nodes {
		t.Add(&p.Nodes[i].Self)
	}
	return t
}

// Inclusive returns, for every node, its self weights plus those of all its
// descendants.
func (p *Profile) Inclusive() []Weights {
	inc := make([]Weights, len(p.Nodes))
	for i := range p.Nodes {
		inc[i] = p.Nodes[i].Self
	}
	// Children always follow their parent, so one reverse pass is enough.
	for i := len(p.Nodes) - 1; i > 0; i-- {
		inc[p.Nodes[i].Parent].Add(&inc[i])
	}
	return inc
}

// FuncStat is the cost of one function across the whole profile.
type FuncStat struct {
	Frame int32
	Self  Weights
	// Total includes callees. A recursive function is counted once per
	// outermost call, so Total never exceeds the profile total.
	Total Weights
}

// Functions aggregates the tree by function, largest self energy first.
func (p *Profile) Functions() []FuncStat {
	inc := p.Inclusive()
	stats := make(map[int32]*FuncStat)
	get := func(f int32) *FuncStat {
		s := stats[f]
		if s == nil {
			s = &FuncStat{Frame: f}
			stats[f] = s
		}
		return s
	}

	// onPath[i] is true when an ancestor of node i has the same frame.
	// Parents precede children, so it can be filled in one forward pass by
	// walking up; stacks are shallow enough for that to be cheap.
	for i := 1; i < len(p.Nodes); i++ {
		n := &p.Nodes[i]
		if p.Frames[n.Frame].Kind != KindFunc {
			continue
		}
		s := get(n.Frame)
		s.Self.Add(&n.Self)
		recursive := false
		for a := n.Parent; a > 0; a = p.Nodes[a].Parent {
			if p.Nodes[a].Frame == n.Frame {
				recursive = true
				break
			}
		}
		if !recursive {
			s.Total.Add(&inc[i])
		}
	}

	out := make([]FuncStat, 0, len(stats))
	for _, s := range stats {
		out = append(out, *s)
	}
	// Largest self energy first. A profile without energy (recorded where
	// the kernel reports none) falls through to CPU time.
	sort.Slice(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		for _, pair := range [][2]uint64{
			{a.Self.Energy(), b.Self.Energy()},
			{a.Total.Energy(), b.Total.Energy()},
			{a.Self.CPU(), b.Self.CPU()},
			{a.Total.CPU(), b.Total.CPU()},
		} {
			if pair[0] != pair[1] {
				return pair[0] > pair[1]
			}
		}
		return p.FrameLabel(a.Frame) < p.FrameLabel(b.Frame)
	})
	return out
}

// FrameLabel is a frame's display name.
func (p *Profile) FrameLabel(f int32) string {
	fr := &p.Frames[f]
	switch fr.Kind {
	case KindProcess:
		return fmt.Sprintf("%s [%d]", fr.Name, fr.PID)
	case KindThread:
		return fr.Name
	}
	return fr.Name
}

// Metric selects which weight a folded export carries.
type Metric int

const (
	MetricEnergy  Metric = iota // microjoules
	MetricCPU                   // microseconds
	MetricSamples               // stack count
)

// ParseMetric maps a command-line name to a Metric.
func ParseMetric(s string) (Metric, error) {
	switch strings.ToLower(s) {
	case "energy", "":
		return MetricEnergy, nil
	case "cpu", "time":
		return MetricCPU, nil
	case "samples":
		return MetricSamples, nil
	}
	return 0, fmt.Errorf("unknown metric %q (want energy, cpu or samples)", s)
}

func (m Metric) value(w *Weights) uint64 {
	switch m {
	case MetricCPU:
		return w.CPU() / 1000
	case MetricSamples:
		return w.Samples
	}
	return w.Energy() / 1000
}

// WriteFolded writes the tree in the "a;b;c value" format understood by
// flamegraph.pl, speedscope and inferno. Energy is in microjoules and CPU
// time in microseconds so the integers stay meaningful for short functions.
func (p *Profile) WriteFolded(w io.Writer, m Metric, withThreads bool) error {
	paths := make([]string, len(p.Nodes))
	merged := make(map[string]uint64)
	var order []string
	for i := 1; i < len(p.Nodes); i++ {
		n := &p.Nodes[i]
		fr := &p.Frames[n.Frame]
		label := strings.ReplaceAll(p.FrameLabel(n.Frame), ";", ":")
		if fr.Kind == KindThread && !withThreads {
			paths[i] = paths[n.Parent]
		} else if paths[n.Parent] == "" {
			paths[i] = label
		} else {
			paths[i] = paths[n.Parent] + ";" + label
		}
		v := m.value(&n.Self)
		if v == 0 || paths[i] == "" {
			continue
		}
		if _, ok := merged[paths[i]]; !ok {
			order = append(order, paths[i])
		}
		merged[paths[i]] += v
	}
	sort.Strings(order)
	for _, path := range order {
		if _, err := fmt.Fprintf(w, "%s %d\n", path, merged[path]); err != nil {
			return err
		}
	}
	return nil
}
