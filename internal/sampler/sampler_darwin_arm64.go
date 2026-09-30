//go:build darwin && arm64

package sampler

/*
#cgo CFLAGS: -O2 -Wall -Wextra
#include <stdlib.h>
#include "wf.h"
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Session owns the native sampler for one recording.
type Session struct {
	s   *C.wf_session
	buf []byte
}

const recordHeaderSize = int(unsafe.Sizeof(C.wf_record{}))

// Supported reports whether this machine can be profiled.
func Supported() error {
	return nil
}

// PerfLevels returns the names of the CPU performance levels, fastest first
// (for example "Super", "Efficiency").
func PerfLevels() []string {
	var names [C.WF_MAX_LEVELS][32]C.char
	n := int(C.wf_perf_levels(&names[0]))
	out := make([]string, n)
	for i := range out {
		out[i] = C.GoString(&names[i][0])
	}
	return out
}

func newSession() (*Session, error) {
	var errbuf [512]C.char
	s := C.wf_new(&errbuf[0], C.size_t(len(errbuf)))
	if s == nil {
		return nil, errors.New(C.GoString(&errbuf[0]))
	}
	return &Session{s: s, buf: make([]byte, 4<<20)}, nil
}

func cStrings(in []string) (**C.char, func()) {
	n := len(in)
	size := C.size_t(n+1) * C.size_t(unsafe.Sizeof((*C.char)(nil)))
	arr := (**C.char)(C.calloc(1, size))
	view := unsafe.Slice(arr, n+1)
	for i, s := range in {
		view[i] = C.CString(s)
	}
	return arr, func() {
		for i := 0; i < n; i++ {
			C.free(unsafe.Pointer(view[i]))
		}
		C.free(unsafe.Pointer(arr))
	}
}

// Launch starts argv with the preload library injected. The new process is
// held before main() until Start is called.
func Launch(argv, env []string, preloadPath string) (*Session, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command given")
	}
	s, err := newSession()
	if err != nil {
		return nil, err
	}
	cargv, freeArgv := cStrings(argv)
	defer freeArgv()
	cenv, freeEnv := cStrings(env)
	defer freeEnv()
	cpath := C.CString(preloadPath)
	defer C.free(unsafe.Pointer(cpath))

	var errbuf [1024]C.char
	if C.wf_launch(s.s, cpath, cargv, cenv, &errbuf[0], C.size_t(len(errbuf))) != 0 {
		msg := C.GoString(&errbuf[0])
		s.Close()
		return nil, errors.New(msg)
	}
	return s, nil
}

// Attach connects to a running process. It needs root.
func Attach(pid int) (*Session, error) {
	s, err := newSession()
	if err != nil {
		return nil, err
	}
	var errbuf [1024]C.char
	if C.wf_attach(s.s, C.pid_t(pid), &errbuf[0], C.size_t(len(errbuf))) != 0 {
		msg := C.GoString(&errbuf[0])
		s.Close()
		return nil, errors.New(msg)
	}
	return s, nil
}

// Start begins sampling and releases the target.
func (s *Session) Start(interval time.Duration, maxDepth int) error {
	var errbuf [512]C.char
	us := interval.Microseconds()
	if C.wf_start(s.s, C.uint32_t(us), C.uint32_t(maxDepth), &errbuf[0], C.size_t(len(errbuf))) != 0 {
		return errors.New(C.GoString(&errbuf[0]))
	}
	return nil
}

// Stop ends sampling and flushes what is still pending. Drain afterwards to
// collect the final records.
func (s *Session) Stop() {
	C.wf_stop(s.s)
}

// Drain calls fn for every record produced since the last call and returns how
// many there were. The record and its Frames are only valid inside fn.
func (s *Session) Drain(fn func(*Record)) int {
	total := 0
	var rec Record
	for {
		n := int(C.wf_drain(s.s, unsafe.Pointer(&s.buf[0]), C.size_t(len(s.buf))))
		if n == 0 {
			return total
		}
		for off := 0; off < n; {
			b := s.buf[off:]
			size := int(binary.LittleEndian.Uint32(b[0:]))
			if size < recordHeaderSize || off+size > n {
				return total
			}
			h := (*C.wf_record)(unsafe.Pointer(&b[0]))
			rec.Target = uint32(h.target)
			rec.PID = int(h.pid)
			rec.TID = uint64(h.tid)
			rec.TimeNs = uint64(h.t_ns)
			rec.LR = uint64(h.lr)
			rec.Flags = uint32(h.flags)
			for m := 0; m < NumWeights; m++ {
				for l := 0; l < MaxLevels; l++ {
					rec.W[m][l] = uint64(h.w[m][l])
				}
			}
			nf := int(h.nframes)
			if cap(rec.Frames) < nf {
				rec.Frames = make([]uint64, nf)
			}
			rec.Frames = rec.Frames[:nf]
			fb := b[recordHeaderSize:size]
			for i := 0; i < nf; i++ {
				rec.Frames[i] = binary.LittleEndian.Uint64(fb[i*8:])
			}
			fn(&rec)
			total++
			off += size
		}
	}
}

// RootExited reports whether the profiled process has ended, and its wait
// status when wattflame launched it.
func (s *Session) RootExited() (bool, ExitStatus) {
	var status C.int
	exited := C.wf_root_exited(s.s, &status) != 0
	ws := syscall.WaitStatus(status)
	if ws.Signaled() {
		return exited, ExitStatus{Signal: ws.Signal().String(), SignalNum: int(ws.Signal())}
	}
	return exited, ExitStatus{Code: ws.ExitStatus()}
}

// RootTreeCPU is the CPU time of the launched program and of every descendant
// it had waited for when it exited, as reported by the kernel on wait. Zero
// until then, and in attach mode.
func (s *Session) RootTreeCPU() time.Duration {
	return time.Duration(C.wf_root_tree_cpu_ns(s.s))
}

// Reap frees the symbol tables of processes that ended a few calls ago. Call
// it after a Drain whose addresses have all been symbolicated.
func (s *Session) Reap() {
	C.wf_reap(s.s)
}

// RootPID is the pid of the launched or attached process.
func (s *Session) RootPID() int {
	return int(C.wf_root_pid(s.s))
}

// Signal delivers sig to the root process.
func (s *Session) Signal(sig os.Signal) {
	if sn, ok := sig.(syscall.Signal); ok {
		C.wf_kill_root(s.s, C.int(sn))
	}
}

// Stats returns sampler bookkeeping.
func (s *Session) Stats() Stats {
	var st C.wf_stats
	C.wf_get_stats(s.s, &st)
	return Stats{
		Ticks:        uint64(st.ticks),
		Overruns:     uint64(st.overruns),
		Samples:      uint64(st.samples),
		SampleErrors: uint64(st.sample_errors),
		SuspendNs:    uint64(st.suspend_ns),
		SuspendMaxNs: uint64(st.suspend_max_ns),
		SelfEnergyNJ: uint64(st.self_energy_nj),
		SelfCPUNs:    uint64(st.self_cpu_ns),
		ElapsedNs:    uint64(st.elapsed_ns),
		Dropped:      uint64(st.dropped),
		TargetsLost:  uint64(st.targets_lost),
		NoCounters:   st.no_counters != 0,
		Targets:      int(st.targets),
		Levels:       int(st.nlevels),
	}
}

// Targets lists every process seen during the recording.
func (s *Session) Targets() []TargetInfo {
	n := int(C.wf_target_count(s.s))
	out := make([]TargetInfo, 0, n)
	for i := 0; i < n; i++ {
		var ti C.wf_target_info
		if C.wf_get_target(s.s, C.uint32_t(i), &ti) == 0 {
			break
		}
		out = append(out, TargetInfo{
			PID:             int(ti.pid),
			Alive:           ti.alive != 0,
			Opaque:          ti.opaque != 0,
			EnergyNJ:        uint64(ti.energy_nj),
			ThreadsEnergyNJ: uint64(ti.threads_energy_nj),
			Name:            C.GoString(&ti.name[0]),
			Path:            C.GoString(&ti.path[0]),
		})
	}
	return out
}

// Symbolicate resolves an address inside a target.
func (s *Session) Symbolicate(target uint32, addr uint64) Symbol {
	var sym C.wf_symbol
	C.wf_symbolicate(s.s, C.uint32_t(target), C.uint64_t(addr), &sym)
	return Symbol{
		Found:      sym.found != 0,
		Stub:       sym.stub != 0,
		Name:       C.GoString(&sym.name[0]),
		Module:     C.GoString(&sym.module[0]),
		ModulePath: C.GoString(&sym.module_path[0]),
		File:       C.GoString(&sym.file[0]),
		Line:       uint32(sym.line),
		Start:      uint64(sym.start),
		Len:        uint64(sym.len),
	}
}

// ThreadName returns the name a thread of a target gave itself, or "". It
// still answers after the thread has ended.
func (s *Session) ThreadName(target uint32, tid uint64) string {
	var buf [128]C.char
	C.wf_thread_name(s.s, C.uint32_t(target), C.uint64_t(tid), &buf[0], C.size_t(len(buf)))
	return C.GoString(&buf[0])
}

// Close releases the session. A launched process that is still running is
// killed.
func (s *Session) Close() {
	if s.s != nil {
		C.wf_free(s.s)
		s.s = nil
	}
}
