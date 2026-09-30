// Package sampler takes stack samples of another process and pairs them with
// the kernel's per-thread energy counters. The implementation is native code
// for macOS on Apple Silicon; other platforms get a stub that reports the
// requirement.
package sampler

// MaxLevels is the largest number of CPU performance levels a record carries.
const MaxLevels = 4

// Indexes into Record.W.
const (
	WEnergyNJ = iota
	WCPUNs
	WCycles
	WInstr
	NumWeights
)

// Record flags.
const (
	FlagTruncated = 1 << 0 // stack was deeper than the limit
	FlagOffCPU    = 1 << 1 // thread was not runnable when the stack was taken
	FlagNoStack   = 1 << 2 // weights with no stack to attach them to
	FlagOpaque    = 1 << 3 // from a process that is counted but cannot be sampled
)

// Record is one stack sample together with the energy, CPU time, cycles and
// instructions attributed to it, per performance level.
type Record struct {
	Target uint32
	PID    int
	TID    uint64
	TimeNs uint64
	LR     uint64
	Flags  uint32
	W      [NumWeights][MaxLevels]uint64
	Frames []uint64 // Frames[0] is the program counter, then return addresses
}

// Symbol describes the function containing an address.
type Symbol struct {
	Found      bool
	Stub       bool // in a dyld shared cache stub island, outside any library
	Name       string
	Module     string
	ModulePath string
	File       string
	Line       uint32
	Start      uint64
	Len        uint64
}

// Stats is the sampler's own bookkeeping for a recording.
type Stats struct {
	Ticks        uint64
	Overruns     uint64
	Samples      uint64
	SampleErrors uint64
	SuspendNs    uint64
	SuspendMaxNs uint64
	SelfEnergyNJ uint64
	SelfCPUNs    uint64
	ElapsedNs    uint64
	Dropped      uint64
	Targets      int
	Levels       int
}

// TargetInfo describes one profiled process.
type TargetInfo struct {
	PID   int
	Alive bool
	// Opaque targets never handed over a task port (system binaries,
	// hardened apps, Rosetta): their energy is known, their stacks are not.
	Opaque bool
	// EnergyNJ is the kernel's own total for the process over the recording
	// and ThreadsEnergyNJ what the per-thread counters added up to when
	// that total was last read.
	EnergyNJ        uint64
	ThreadsEnergyNJ uint64
	Name            string
	Path            string // executable path
}
