package profile

import (
	"testing"

	"github.com/useless-husband/wattflame/internal/sampler"
)

// fakeSym is a tiny address space: each function occupies 0x100 bytes.
type fakeSym struct {
	funcs   map[uint64]sampler.Symbol // keyed by function start
	threads map[uint64]string
	calls   int
}

func (f *fakeSym) Symbolicate(target uint32, addr uint64) sampler.Symbol {
	f.calls++
	start := addr &^ 0xff
	if s, ok := f.funcs[start]; ok {
		s.Start = start
		s.Len = 0x100
		if s.Found {
			s.Line = uint32(addr&0xff) + 1
		}
		return s
	}
	return sampler.Symbol{}
}

func (f *fakeSym) ThreadName(target uint32, tid uint64) string { return f.threads[tid] }

const (
	aStart  = 0x1000 // start (app)
	aMain   = 0x1100 // main (app)
	aWork   = 0x1200 // work (app)
	aLeaf   = 0x1300 // leaf (app), frameless
	aMemcpy = 0x2000 // memcpy (system)
	aStub   = 0x3000 // shared cache stub
	aStrip  = 0x4000 // inside a stripped library
	aJIT    = 0x9000 // nothing known
)

func newFake() *fakeSym {
	app := "/work/app"
	return &fakeSym{
		funcs: map[uint64]sampler.Symbol{
			aStart:  {Found: true, Name: "start", Module: "app", ModulePath: app},
			aMain:   {Found: true, Name: "main", Module: "app", ModulePath: app, File: "/src/main.c"},
			aWork:   {Found: true, Name: "work", Module: "app", ModulePath: app, File: "/src/work.c"},
			aLeaf:   {Found: true, Name: "leaf", Module: "app", ModulePath: app, File: "/src/work.c"},
			aMemcpy: {Found: true, Name: "memcpy", Module: "libsystem_platform.dylib", ModulePath: "/usr/lib/system/libsystem_platform.dylib"},
			aStub:   {Stub: true, Module: "dyld shared cache", ModulePath: "/System/Library/dyld/"},
			aStrip:  {Module: "libvendor.dylib", ModulePath: "/opt/vendor/libvendor.dylib"},
		},
		threads: map[uint64]string{7: "worker"},
	}
}

// rec builds a record with energy on level 0. Frames are leaf first; every
// frame after the first is a return address (function start + 0x10).
func rec(tid uint64, energy uint64, lr uint64, frames ...uint64) *sampler.Record {
	r := &sampler.Record{Target: 0, PID: 42, TID: tid, LR: lr, Frames: frames}
	r.W[sampler.WEnergyNJ][0] = energy
	r.W[sampler.WCPUNs][0] = energy / 2
	return r
}

// path returns the frame names from the root down to node i.
func path(p *Profile, i int32) []string {
	var out []string
	for ; i > 0; i = p.Nodes[i].Parent {
		out = append([]string{p.Frames[p.Nodes[i].Frame].Name}, out...)
	}
	return out
}

// leafPaths maps "a;b;c" to self energy for every node that has any.
func leafPaths(p *Profile) map[string]uint64 {
	out := make(map[string]uint64)
	for i := range p.Nodes {
		if e := p.Nodes[i].Self.Energy(); e > 0 {
			key := ""
			for j, name := range path(p, int32(i)) {
				if j > 0 {
					key += ";"
				}
				key += name
			}
			out[key] += e
		}
	}
	return out
}

func build(t *testing.T, recs ...*sampler.Record) *Profile {
	t.Helper()
	b := NewBuilder(newFake())
	b.SetProcess(0, "app", "/work/app")
	for _, r := range recs {
		b.Add(r)
	}
	p := b.Finish(Meta{}, []Level{{Name: "P"}, {Name: "E"}})
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return p
}

func wantPaths(t *testing.T, p *Profile, want map[string]uint64) {
	t.Helper()
	got := leafPaths(p)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("path %q: got %d nJ, want %d", k, got[k], v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected path %q with %d nJ", k, v)
		}
	}
}

func TestBuilderStacksAreRootFirst(t *testing.T) {
	p := build(t, rec(1, 100, 0, aWork+0x20, aMain+0x10, aStart+0x10))
	wantPaths(t, p, map[string]uint64{"app;main thread;start;main;work": 100})
}

func TestBuilderMergesIdenticalStacks(t *testing.T) {
	p := build(t,
		rec(1, 100, 0, aWork+0x20, aMain+0x10, aStart+0x10),
		rec(1, 50, 0, aWork+0x44, aMain+0x10, aStart+0x10),
	)
	wantPaths(t, p, map[string]uint64{"app;main thread;start;main;work": 150})
	total := p.Total()
	if total.Samples != 2 || total.Energy() != 150 || total.CPU() != 75 {
		t.Errorf("totals = %+v", total)
	}
}

// A return address is the instruction after the call. When the call is the
// last instruction of its function, that address already belongs to the next
// function; the builder must still name the caller.
func TestBuilderReturnAddressAtFunctionEnd(t *testing.T) {
	p := build(t, rec(1, 10, 0, aWork+0x20, aMain+0x100, aStart+0x10))
	wantPaths(t, p, map[string]uint64{"app;main thread;start;main;work": 10})
}

func TestBuilderLeafCaller(t *testing.T) {
	tests := []struct {
		name string
		r    *sampler.Record
		want string
	}{
		{
			// leaf never set up a frame: the walk yields main, lr says work.
			name: "frameless leaf gets its caller from lr",
			r:    rec(1, 10, aWork+0x30, aLeaf+0x08, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;work;leaf",
		},
		{
			// work has a frame and has not called anything yet: lr still holds
			// its own return address, the one the walk found.
			name: "lr equal to the first return address is ignored",
			r:    rec(1, 10, aMain+0x10, aWork+0x20, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;work",
		},
		{
			// work called something that returned: lr points back into work.
			name: "stale lr inside the same function is ignored",
			r:    rec(1, 10, aWork+0x40, aWork+0x44, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;work",
		},
		{
			name: "lr at the very end of the same function is ignored",
			r:    rec(1, 10, aWork+0x100, aWork+0x44, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;work",
		},
		{
			name: "lr into unknown memory is ignored",
			r:    rec(1, 10, aJIT+0x10, aLeaf+0x08, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;leaf",
		},
		{
			name: "no lr",
			r:    rec(1, 10, 0, aLeaf+0x08, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;leaf",
		},
		{
			// A shared cache stub has no symbol and no frame; its caller is
			// always in lr.
			name: "stub gets its caller from lr",
			r:    rec(1, 10, aMemcpy+0x30, aStub+0x04, aWork+0x10, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;work;memcpy;[shared cache stub]",
		},
		{
			// Without function bounds there is no way to tell a real caller
			// from a stale lr, so a stripped leaf is left alone.
			name: "stripped leaf does not trust lr",
			r:    rec(1, 10, aWork+0x30, aStrip+0x08, aMain+0x10, aStart+0x10),
			want: "app;main thread;start;main;[libvendor.dylib]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := build(t, tt.r)
			wantPaths(t, p, map[string]uint64{tt.want: 10})
		})
	}
}

func TestBuilderUnknownAndNoStack(t *testing.T) {
	noStack := rec(1, 7, 0)
	noStack.Flags = sampler.FlagNoStack
	p := build(t,
		rec(1, 5, 0, aJIT+0x10, aMain+0x10, aStart+0x10),
		noStack,
	)
	wantPaths(t, p, map[string]uint64{
		"app;main thread;start;main;[unknown]": 5,
		"app;main thread;[no stacks]":          7,
	})
}

func TestBuilderOpaqueProcesses(t *testing.T) {
	opaque := func(target uint32, pid int, tid, energy uint64) *sampler.Record {
		r := rec(tid, energy, 0)
		r.Target, r.PID = target, pid
		r.Flags = sampler.FlagNoStack | sampler.FlagOpaque
		return r
	}
	b := NewBuilder(newFake())
	b.SetProcess(0, "app", "/work/app")
	b.SetProcess(1, "clang", "/Library/Developer/CommandLineTools/usr/bin/clang")
	// Target 0 is counted while it starts up, then sampled.
	b.Add(opaque(0, 42, 1, 3))
	b.Add(rec(1, 100, 0, aWork+0x20, aMain+0x10))
	b.Add(opaque(0, 42, 1, 2))
	// Target 1 can never be sampled.
	b.Add(opaque(1, 77, 5, 500))
	// A thread of target 0 that ended unseen is not "startup".
	gone := rec(9, 4, 0)
	gone.Flags = sampler.FlagNoStack
	b.Add(gone)
	p := b.Finish(Meta{}, nil)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, p, map[string]uint64{
		"app;main thread;[process startup]": 5,
		"app;main thread;main;work":         100,
		"app;thread 9;[no stacks]":          4,
		"clang;main thread;[no stacks]":     500,
	})
}

func TestBuilderThreadsAndProcesses(t *testing.T) {
	b := NewBuilder(newFake())
	b.SetProcess(0, "app", "/work/app")
	b.Add(rec(3, 10, 0, aWork+0x20))
	b.Add(rec(7, 20, 0, aWork+0x20)) // named "worker" by the fake
	b.Add(rec(9, 30, 0, aWork+0x20))
	other := rec(5, 40, 0, aWork+0x20)
	other.Target = 1
	other.PID = 99
	b.Add(other) // SetProcess never called for target 1
	p := b.Finish(Meta{}, nil)

	wantPaths(t, p, map[string]uint64{
		"app;main thread;work":    10,
		"app;worker;work":         20,
		"app;thread 9;work":       30,
		"pid 99;main thread;work": 40,
	})
	for _, fr := range p.Frames {
		switch fr.Kind {
		case KindProcess:
			if fr.PID == 0 {
				t.Errorf("process frame %q has no pid", fr.Name)
			}
		case KindThread:
			if fr.TID == 0 {
				t.Errorf("thread frame %q has no tid", fr.Name)
			}
		}
	}
}

func TestBuilderOrigin(t *testing.T) {
	p := build(t,
		rec(1, 1, 0, aMemcpy+0x10, aWork+0x10),
		rec(1, 1, 0, aStrip+0x10, aWork+0x10),
		rec(1, 1, aWork+0x30, aStub+0x04, aWork+0x10),
		rec(1, 1, 0, aJIT+0x10, aWork+0x10),
	)
	want := map[string]string{
		"work":                OriginApp,
		"memcpy":              OriginSystem,
		"[libvendor.dylib]":   OriginLibrary,
		"[shared cache stub]": OriginSystem,
		"[unknown]":           "",
	}
	seen := 0
	for _, fr := range p.Frames {
		if w, ok := want[fr.Name]; ok {
			seen++
			if fr.Origin != w {
				t.Errorf("origin of %s = %q, want %q", fr.Name, fr.Origin, w)
			}
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of %d expected frames", seen, len(want))
	}
}

func TestBuilderOriginThroughSymlink(t *testing.T) {
	// Homebrew installs /opt/homebrew/bin/tool as a symlink into the Cellar:
	// the kernel names the real file, the loader the link.
	f := newFake()
	f.funcs[aWork] = sampler.Symbol{Found: true, Name: "work", Module: "tool", ModulePath: "/opt/homebrew/bin/tool"}
	b := NewBuilder(f)
	b.SetProcess(0, "tool", "/opt/homebrew/Cellar/tool/1.2/bin/tool")
	b.Add(rec(1, 1, 0, aWork+0x20))
	p := b.Finish(Meta{}, nil)
	for _, fr := range p.Frames {
		if fr.Name == "work" && fr.Origin != OriginApp {
			t.Errorf("origin = %q, want %q", fr.Origin, OriginApp)
		}
	}
}

func TestBuilderHotLines(t *testing.T) {
	// The fake reports line = offset + 1.
	p := build(t,
		rec(1, 100, 0, aWork+0x09, aMain+0x10),
		rec(1, 300, 0, aWork+0x13, aMain+0x10),
		rec(1, 50, 0, aWork+0x09, aMain+0x10),
	)
	var work *Frame
	for i := range p.Frames {
		if p.Frames[i].Name == "work" {
			work = &p.Frames[i]
		}
	}
	if work == nil {
		t.Fatal("no work frame")
	}
	if len(work.Lines) != 2 || work.Lines[0] != (LineCost{Line: 0x14, EnergyNJ: 300}) || work.Lines[1] != (LineCost{Line: 0x0a, EnergyNJ: 150}) {
		t.Errorf("lines = %+v", work.Lines)
	}
	if work.File != "/src/work.c" {
		t.Errorf("file = %q", work.File)
	}
}

func TestBuilderCachesSymbolLookups(t *testing.T) {
	f := newFake()
	b := NewBuilder(f)
	for i := 0; i < 100; i++ {
		b.Add(rec(1, 1, 0, aWork+0x20, aMain+0x10, aStart+0x10))
	}
	if f.calls != 3 {
		t.Errorf("symbolizer called %d times for 3 distinct addresses", f.calls)
	}
	if b.Records() != 100 {
		t.Errorf("Records = %d", b.Records())
	}
}

func TestBuilderTimeline(t *testing.T) {
	b := NewBuilder(newFake())
	for i := 0; i < 1000; i++ {
		r := rec(1, 10, 0, aWork+0x20)
		r.W[sampler.WEnergyNJ][1] = 1
		r.TimeNs = uint64(i) * 10 * 1000 * 1000 // one per 10 ms bucket
		b.Add(r)
	}
	p := b.Finish(Meta{}, []Level{{Name: "P"}, {Name: "E"}})
	tl := p.Timeline
	if len(tl.EnergyNJ) > timelineMaxPoints || len(tl.EnergyNJ) < timelineMaxPoints/2 {
		t.Fatalf("timeline has %d points", len(tl.EnergyNJ))
	}
	if tl.BucketNs != 3*timelineBucketNs {
		t.Errorf("bucket = %d ns", tl.BucketNs)
	}
	var sum [2]uint64
	for _, row := range tl.EnergyNJ {
		if len(row) != 2 {
			t.Fatalf("row has %d levels", len(row))
		}
		sum[0] += row[0]
		sum[1] += row[1]
	}
	if sum != [2]uint64{10000, 1000} {
		t.Errorf("timeline sums to %v", sum)
	}
}

func TestBuilderEmpty(t *testing.T) {
	p := NewBuilder(newFake()).Finish(Meta{}, nil)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(p.Nodes) != 1 || p.Frames == nil || p.Timeline.EnergyNJ == nil || p.Meta.Processes == nil {
		t.Errorf("empty profile is not well formed: %+v", p)
	}
	if got := p.Total(); !got.IsZero() {
		t.Errorf("total = %+v", got)
	}
	if fs := p.Functions(); len(fs) != 0 {
		t.Errorf("functions = %v", fs)
	}
}
