package profile

import (
	"fmt"
	pathpkg "path"
	"sort"
	"strings"

	"github.com/useless-husband/wattflame/internal/demangle"
	"github.com/useless-husband/wattflame/internal/sampler"
)

// Symbolizer turns what the sampler saw into names. *sampler.Session
// implements it; tests supply a fake.
type Symbolizer interface {
	Symbolicate(target uint32, addr uint64) sampler.Symbol
	ThreadName(pid int, tid uint64) string
}

// Names of the synthetic frames.
const (
	// Code in the dyld shared cache's stub islands.
	stubFrameName = "[shared cache stub]"
	// Energy with no stack: the process could be counted but not sampled
	// (a macOS system binary, a hardened app, an Intel binary under
	// Rosetta), or a thread ended before it was ever seen running.
	noStackFrameName = "[no stacks]"
	// The part of a sampled process that ran before it could be sampled:
	// the dynamic loader mapping libraries, up to the handshake.
	startupFrameName = "[process startup]"
)

const (
	timelineBucketNs  = 10 * 1000 * 1000 // resolution while recording
	timelineMaxPoints = 400              // resolution of the saved profile
	maxLinesPerFrame  = 8
)

type frameKey struct {
	kind   string
	module string
	name   string
	id     uint64 // pid or tid for the synthetic levels
	target uint32
}

type childKey struct {
	parent int32
	frame  int32
}

type addrKey struct {
	target uint32
	addr   uint64
}

type resolved struct {
	frame    int32
	line     uint32
	start    uint64
	length   uint64
	found    bool // a function symbol was found
	hasOwner bool // the address is inside a known image
	stub     bool // a shared cache stub: no frame of its own, caller in lr
}

type threadKey struct {
	target uint32
	tid    uint64
}

// Builder folds sampler records into a call tree as they arrive.
type Builder struct {
	sym Symbolizer

	frames   []Frame
	frameIdx map[frameKey]int32
	nodes    []Node
	children map[childKey]int32
	addrs    map[addrKey]resolved
	lines    map[int32]map[uint32]uint64

	procNode   map[uint32]int32
	threadNode map[threadKey]int32
	threadPID  map[threadKey]int
	sampled    map[uint32]bool    // targets with at least one real stack
	unsampled  map[uint32][]int32 // per target, its "[no stacks]" nodes from opaque records
	procNames  map[uint32]string
	procPaths  map[uint32]string

	timeline [][MaxLevels]uint64
	records  uint64
	scratch  []int32
}

// NewBuilder returns an empty builder.
func NewBuilder(sym Symbolizer) *Builder {
	return &Builder{
		sym:        sym,
		frameIdx:   make(map[frameKey]int32),
		nodes:      []Node{{Parent: -1, Frame: -1}},
		children:   make(map[childKey]int32),
		addrs:      make(map[addrKey]resolved),
		lines:      make(map[int32]map[uint32]uint64),
		procNode:   make(map[uint32]int32),
		threadNode: make(map[threadKey]int32),
		threadPID:  make(map[threadKey]int),
		sampled:    make(map[uint32]bool),
		unsampled:  make(map[uint32][]int32),
		procNames:  make(map[uint32]string),
		procPaths:  make(map[uint32]string),
	}
}

// SetProcess records a target's name and executable path. Call it before the
// first record of that target arrives: the name labels the process, and the
// path decides which frames count as the program's own code.
func (b *Builder) SetProcess(target uint32, name, exePath string) {
	b.procNames[target] = name
	b.procPaths[target] = exePath
	if n, ok := b.procNode[target]; ok {
		b.frames[b.nodes[n].Frame].Name = name
	}
}

// Records is the number of records added so far.
func (b *Builder) Records() uint64 { return b.records }

func (b *Builder) frame(k frameKey, f Frame) int32 {
	if i, ok := b.frameIdx[k]; ok {
		return i
	}
	i := int32(len(b.frames))
	b.frames = append(b.frames, f)
	b.frameIdx[k] = i
	return i
}

func (b *Builder) child(parent, frame int32) int32 {
	k := childKey{parent, frame}
	if i, ok := b.children[k]; ok {
		return i
	}
	i := int32(len(b.nodes))
	b.nodes = append(b.nodes, Node{Parent: parent, Frame: frame})
	b.children[k] = i
	return i
}

func (b *Builder) resolve(target uint32, addr uint64) resolved {
	k := addrKey{target, addr}
	if r, ok := b.addrs[k]; ok {
		return r
	}
	s := b.sym.Symbolicate(target, addr)
	if s.Found {
		s.Name = demangle.Name(s.Name)
	}
	var r resolved
	r.line = s.Line
	r.start = s.Start
	r.length = s.Len
	r.found = s.Found
	r.hasOwner = s.Module != ""
	r.stub = s.Stub
	switch {
	case s.Stub:
		r.frame = b.frame(frameKey{module: s.Module, name: stubFrameName},
			Frame{Name: stubFrameName, Module: s.Module, Origin: OriginSystem})
	case s.Found:
		r.frame = b.frame(frameKey{module: s.Module, name: s.Name},
			Frame{Name: s.Name, Module: s.Module, File: s.File, Origin: b.origin(target, s.ModulePath)})
		if fr := &b.frames[r.frame]; fr.File == "" && s.File != "" {
			fr.File = s.File
		}
	case s.Module != "":
		// Inside a known image but between symbols (stripped binary).
		name := "[" + s.Module + "]"
		r.frame = b.frame(frameKey{module: s.Module, name: name},
			Frame{Name: name, Module: s.Module, Origin: b.origin(target, s.ModulePath)})
	default:
		// JIT-compiled code, or an image that was unloaded before it could
		// be looked up.
		r.frame = b.frame(frameKey{name: "[unknown]"}, Frame{Name: "[unknown]"})
	}
	b.addrs[k] = r
	return r
}

// origin classifies an image: the program's own executable, part of macOS, or
// some other library.
func (b *Builder) origin(target uint32, modulePath string) string {
	switch {
	case modulePath == "":
		return ""
	case modulePath == b.procPaths[target], pathpkg.Base(modulePath) == pathpkg.Base(b.procPaths[target]):
		// The kernel reports the executable's resolved path while the loader
		// may have seen it through a symlink, so the file name decides when
		// the full paths differ.
		return OriginApp
	case strings.HasPrefix(modulePath, "/usr/lib/"),
		strings.HasPrefix(modulePath, "/System/"),
		strings.HasPrefix(modulePath, "/Library/Apple/"):
		return OriginSystem
	}
	return OriginLibrary
}

func (b *Builder) processNode(r *sampler.Record) int32 {
	if n, ok := b.procNode[r.Target]; ok {
		return n
	}
	name := b.procNames[r.Target]
	if name == "" {
		name = fmt.Sprintf("pid %d", r.PID)
	}
	f := b.frame(frameKey{kind: KindProcess, target: r.Target, id: uint64(r.PID)},
		Frame{Kind: KindProcess, Name: name, PID: r.PID})
	n := b.child(0, f)
	b.procNode[r.Target] = n
	return n
}

func (b *Builder) threadNodeFor(r *sampler.Record) int32 {
	k := threadKey{r.Target, r.TID}
	if n, ok := b.threadNode[k]; ok {
		return n
	}
	name := b.sym.ThreadName(r.PID, r.TID)
	f := b.frame(frameKey{kind: KindThread, target: r.Target, id: r.TID},
		Frame{Kind: KindThread, Name: name, PID: r.PID, TID: r.TID})
	n := b.child(b.processNode(r), f)
	b.threadNode[k] = n
	b.threadPID[k] = r.PID
	return n
}

// Add folds one record into the tree.
func (b *Builder) Add(r *sampler.Record) {
	b.records++
	node := b.threadNodeFor(r)
	var energy uint64
	for l := 0; l < MaxLevels; l++ {
		energy += r.W[sampler.WEnergyNJ][l]
	}

	var leaf resolved
	if len(r.Frames) == 0 || r.Flags&sampler.FlagNoStack != 0 {
		f := b.frame(frameKey{name: noStackFrameName}, Frame{Name: noStackFrameName})
		before := len(b.nodes)
		node = b.child(node, f)
		if r.Flags&sampler.FlagOpaque != 0 && len(b.nodes) > before {
			b.unsampled[r.Target] = append(b.unsampled[r.Target], node)
		}
	} else {
		b.sampled[r.Target] = true
		// Frames arrive leaf first. Return addresses point at the
		// instruction after the call, which can already belong to the next
		// function, so look them up one byte earlier.
		path := b.scratch[:0]
		leaf = b.resolve(r.Target, r.Frames[0])
		path = append(path, leaf.frame)
		if caller, ok := b.leafCaller(r, leaf); ok {
			path = append(path, caller)
		}
		for _, addr := range r.Frames[1:] {
			path = append(path, b.resolve(r.Target, addr-1).frame)
		}
		for i := len(path) - 1; i >= 0; i-- {
			node = b.child(node, path[i])
		}
		b.scratch = path
	}

	self := &b.nodes[node].Self
	self.Samples++
	for l := 0; l < MaxLevels; l++ {
		self.EnergyNJ[l] += r.W[sampler.WEnergyNJ][l]
		self.CPUNs[l] += r.W[sampler.WCPUNs][l]
		self.Cycles[l] += r.W[sampler.WCycles][l]
		self.Instr[l] += r.W[sampler.WInstr][l]
	}

	if leaf.found && leaf.line != 0 && energy != 0 {
		m := b.lines[leaf.frame]
		if m == nil {
			m = make(map[uint32]uint64)
			b.lines[leaf.frame] = m
		}
		m[leaf.line] += energy
	}

	bucket := int(r.TimeNs / timelineBucketNs)
	for len(b.timeline) <= bucket {
		b.timeline = append(b.timeline, [MaxLevels]uint64{})
	}
	for l := 0; l < MaxLevels; l++ {
		b.timeline[bucket][l] += r.W[sampler.WEnergyNJ][l]
	}
}

// leafCaller recovers the caller that a frame-pointer walk misses.
//
// The walk starts from the frame record the frame pointer refers to. While a
// function is in its prologue or epilogue, or when it is a leaf that never
// sets up a frame, that record still belongs to its caller, so the walk yields
// the caller's caller and skips the caller itself. In exactly those situations
// the link register holds the return address into the caller.
//
// When the function does have its own frame, the link register is either the
// same return address the walk already found, or a leftover pointing back into
// the function itself (from the last call it made). Both are recognised and
// ignored.
func (b *Builder) leafCaller(r *sampler.Record, leaf resolved) (int32, bool) {
	lr := r.LR
	if lr == 0 {
		return 0, false
	}
	if len(r.Frames) > 1 && r.Frames[1] == lr {
		return 0, false
	}
	switch {
	case leaf.stub:
		// A stub is a few instructions that jump on; it never has a frame.
	case !leaf.found || leaf.length == 0:
		return 0, false
	case lr > leaf.start && lr <= leaf.start+leaf.length:
		return 0, false
	}
	caller := b.resolve(r.Target, lr-1)
	if !caller.hasOwner {
		return 0, false
	}
	return caller.frame, true
}

// Finish names the threads, ranks hot lines, thins the timeline and returns
// the profile. The builder must not be used afterwards.
func (b *Builder) Finish(meta Meta, levels []Level) *Profile {
	// A process that was counted first and sampled later was simply starting
	// up. Say so, instead of leaving an unexplained "[no stacks]" next to its
	// stacks.
	for target, nodes := range b.unsampled {
		if !b.sampled[target] {
			continue
		}
		f := b.frame(frameKey{name: startupFrameName}, Frame{Name: startupFrameName, Origin: OriginSystem})
		for _, n := range nodes {
			b.nodes[n].Frame = f
		}
	}

	// Thread names: whatever the thread called itself, else "main thread"
	// for the first thread of each process and "thread <tid>" for the rest.
	firstTID := make(map[uint32]uint64)
	for k := range b.threadNode {
		if cur, ok := firstTID[k.target]; !ok || k.tid < cur {
			firstTID[k.target] = k.tid
		}
	}
	for k, n := range b.threadNode {
		fr := &b.frames[b.nodes[n].Frame]
		if fr.Name == "" {
			fr.Name = b.sym.ThreadName(b.threadPID[k], k.tid)
		}
		if fr.Name == "" {
			if firstTID[k.target] == k.tid {
				fr.Name = "main thread"
			} else {
				fr.Name = fmt.Sprintf("thread %d", k.tid)
			}
		}
	}

	for f, m := range b.lines {
		lines := make([]LineCost, 0, len(m))
		for line, nj := range m {
			lines = append(lines, LineCost{Line: line, EnergyNJ: nj})
		}
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].EnergyNJ != lines[j].EnergyNJ {
				return lines[i].EnergyNJ > lines[j].EnergyNJ
			}
			return lines[i].Line < lines[j].Line
		})
		if len(lines) > maxLinesPerFrame {
			lines = lines[:maxLinesPerFrame]
		}
		b.frames[f].Lines = lines
	}

	step := (len(b.timeline) + timelineMaxPoints - 1) / timelineMaxPoints
	if step < 1 {
		step = 1
	}
	nl := len(levels)
	if nl == 0 {
		nl = 1
	}
	tl := Timeline{BucketNs: uint64(step) * timelineBucketNs}
	for i := 0; i < len(b.timeline); i += step {
		row := make([]uint64, nl)
		for j := i; j < i+step && j < len(b.timeline); j++ {
			for l := 0; l < nl && l < MaxLevels; l++ {
				row[l] += b.timeline[j][l]
			}
		}
		tl.EnergyNJ = append(tl.EnergyNJ, row)
	}
	if tl.EnergyNJ == nil {
		tl.EnergyNJ = [][]uint64{}
	}

	if meta.Processes == nil {
		meta.Processes = []Process{}
	}
	frames := b.frames
	if frames == nil {
		frames = []Frame{}
	}
	return &Profile{
		Format:   formatName,
		Version:  formatVersion,
		Meta:     meta,
		Levels:   levels,
		Frames:   frames,
		Nodes:    b.nodes,
		Timeline: tl,
	}
}
