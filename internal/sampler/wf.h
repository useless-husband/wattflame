// wattflame sampler: out-of-process stack sampling joined with the kernel's
// per-thread energy counters. See sampler.c for how the two are combined.

#ifndef WATTFLAME_WF_H
#define WATTFLAME_WF_H

#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>

// Upper bound on CPU performance levels (hw.nperflevels). Apple Silicon has 2
// today (M5: "Super" + "Efficiency"); 4 leaves room.
#define WF_MAX_LEVELS 4
#define WF_MAX_DEPTH 512

// Indexes into wf_record.w.
enum {
	WF_W_ENERGY_NJ = 0,
	WF_W_CPU_NS = 1,
	WF_W_CYCLES = 2,
	WF_W_INSTR = 3,
	WF_W_COUNT = 4,
};

// wf_record.flags
enum {
	WF_F_TRUNCATED = 1u << 0, // stack deeper than max_depth
	WF_F_OFFCPU = 1u << 1,    // thread was not runnable when sampled
	WF_F_NOSTACK = 1u << 2,   // weights with no stack (thread or task already gone)
	WF_F_OPAQUE = 1u << 3,    // from a process that can be counted but not sampled
	WF_F_STARTUP = 1u << 4,   // used by a process before it could be sampled
	WF_F_RESIDUAL = 1u << 5,  // billed to the process but read from no thread
	WF_F_TOPUP = 1u << 6,     // more weight for a stack that was already emitted
};

// One stack sample with the share of energy / CPU time / cycles / instructions
// attributed to it, per performance level. Records are variable length and
// packed back to back in the drain buffer.
typedef struct {
	uint32_t size; // total bytes including frames
	uint32_t target;
	int32_t pid;
	uint32_t nframes;
	uint64_t tid;
	uint64_t t_ns; // when the stack was taken, relative to session start
	uint64_t lr;   // link register at sample time, 0 if unknown
	uint32_t flags;
	uint32_t reserved;
	uint64_t w[WF_W_COUNT][WF_MAX_LEVELS];
	uint64_t frames[]; // frames[0] = pc, then return addresses, leaf first
} wf_record;

typedef struct {
	uint64_t ticks;
	uint64_t overruns;      // ticks skipped because the sampler fell behind
	uint64_t samples;       // stacks taken
	uint64_t sample_errors; // suspend / get_state failures
	uint64_t suspend_ns;    // total time target threads spent suspended
	uint64_t suspend_max_ns;
	uint64_t self_energy_nj; // energy used by the sampler thread itself
	uint64_t self_cpu_ns;
	uint64_t elapsed_ns;
	uint64_t dropped; // records discarded because the buffer limit was hit
	uint64_t targets_lost; // processes that could not be tracked (out of memory)
	uint32_t targets;
	uint32_t nlevels;
} wf_stats;

typedef struct {
	uint32_t found;
	uint32_t stub; // address is in a dyld shared cache stub island
	uint32_t line;
	uint32_t reserved;
	uint64_t start; // function range, for the leaf-frame heuristic
	uint64_t len;
	char name[2048];
	char module[256];
	char module_path[1024];
	char file[1024];
} wf_symbol;

typedef struct {
	int32_t pid;
	int32_t alive;
	int32_t opaque; // never handed over a task port: energy only, no stacks
	int32_t reserved;
	// The kernel's per-process energy total (ri_energy_nj) over the recording,
	// and what the per-thread counters added up to at the moment that total
	// was last read. The two are taken together so they can be compared.
	uint64_t energy_nj;
	uint64_t threads_energy_nj;
	char name[256];
	char path[1024]; // executable path
} wf_target_info;

typedef struct wf_session wf_session;

// Create an empty session. Returns NULL and fills err on failure.
wf_session *wf_new(char *err, size_t errlen);

// Spawn argv with the preload library injected. A program that loads the
// library waits in its handshake, before main(), until wf_start. One that
// cannot is still recorded, but only as per-process energy.
int wf_launch(wf_session *s, const char *preload_path, char *const argv[], char *const envp[],
              char *err, size_t errlen);

// Attach to a running process. Needs root (task_for_pid).
int wf_attach(wf_session *s, pid_t pid, char *err, size_t errlen);

// Start / stop the sampling thread. wf_stop flushes everything still pending.
int wf_start(wf_session *s, uint32_t interval_us, uint32_t max_depth, char *err, size_t errlen);
void wf_stop(wf_session *s);

// Copy out whole records accumulated since the last call. Returns bytes written.
size_t wf_drain(wf_session *s, void *buf, size_t cap);

// Free the symbol tables of processes that ended a few calls ago. Call it
// after every address obtained from wf_drain has been passed to
// wf_symbolicate.
void wf_reap(wf_session *s);

// 1 once the root process has exited; *status receives the wait status when it
// was our child (launch mode), otherwise 0.
int wf_root_exited(wf_session *s, int *status);

// CPU time, in nanoseconds, of the launched program and of every descendant
// that had been waited for when it exited (from wait4). 0 in attach mode or
// while it is still running. An independent measure of how much of the
// process tree the recording saw.
uint64_t wf_root_tree_cpu_ns(wf_session *s);
pid_t wf_root_pid(wf_session *s);

void wf_get_stats(wf_session *s, wf_stats *out);
int wf_target_count(wf_session *s);
int wf_get_target(wf_session *s, uint32_t target, wf_target_info *out);

// Resolve an address in a target. Returns 1 if a symbol was found.
int wf_symbolicate(wf_session *s, uint32_t target, uint64_t addr, wf_symbol *out);

// Thread name for a tid, or empty string.
void wf_thread_name(int32_t pid, uint64_t tid, char *out, size_t outlen);

// Number of performance levels and their sysctl names ("Performance", ...).
int wf_perf_levels(char names[WF_MAX_LEVELS][32]);

// Deliver a signal to the root process (used to forward Ctrl-C in attach-less setups).
void wf_kill_root(wf_session *s, int sig);

void wf_free(wf_session *s);

#endif
