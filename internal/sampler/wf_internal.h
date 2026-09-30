#ifndef WATTFLAME_WF_INTERNAL_H
#define WATTFLAME_WF_INTERNAL_H

#include "wf.h"

#include <mach/mach.h>
#include <mach/mach_time.h>
#include <pthread.h>

// CoreSymbolication handle (two opaque words, passed by value).
typedef struct {
	void *data;
	void *obj;
} wf_cs_ref;

wf_cs_ref wf_cs_create(task_t task);
void wf_cs_release(wf_cs_ref ref);
int wf_cs_is_null(wf_cs_ref ref);

typedef struct {
	thread_act_t port;
	uint64_t tid;
	int mark;
	int have_prev;
	uint64_t prev[WF_W_COUNT][WF_MAX_LEVELS]; // last counter values read
	uint64_t acc[WF_W_COUNT][WF_MAX_LEVELS];  // deltas not yet attributed to a stack
	uint64_t acc_energy;                       // energy part of acc, all levels
	uint32_t deferred;                         // ticks acc has waited for a stack
	int had_energy;                            // some energy has been attributed to it
	int gone;                                  // seen to have exited; awaiting removal
	uint32_t npend;                            // stacks waiting for their share
	uint64_t *pend;
	size_t pend_len;
	size_t pend_cap;
	uint64_t *last; // most recent stack, same layout as one pend entry
	size_t last_len;
	size_t last_cap;
} wf_thread;

// Counter values handed from a process image to the one that replaces it on
// exec. The kernel copies the calling thread's counters into the new image's
// first thread, so the new entry must start from where the old one stopped or
// the old image's energy would be counted twice.
#define WF_CARRY_MAX 8
typedef struct {
	int n; // 0 = nothing to carry
	uint64_t prev[WF_CARRY_MAX][WF_W_COUNT][WF_MAX_LEVELS];
	uint64_t energy;  // the process totals at the old image's last reading
	uint64_t penergy;
} wf_carry;

typedef struct {
	uint32_t index;
	pid_t pid;
	task_t task; // MACH_PORT_NULL for an opaque target
	int alive;
	int opaque;    // tracked by pid only: energy but no stacks
	int from_zero; // process began during the recording; count all its energy
	int listed;    // initial thread listing done
	uint64_t thread_hash;
	wf_thread *th;
	int nth;
	int capth;
	mach_port_t pending_reply;
	// Process totals from the kernel (ri_energy_nj and its performance-core
	// part), at the start and at the latest reading, and what the per-thread
	// counters had added up to, per level, at that same reading.
	uint64_t energy_start;
	uint64_t energy_last;
	uint64_t penergy_start;
	uint64_t penergy_last;
	uint64_t seen[WF_MAX_LEVELS];
	uint64_t seen_at_last[WF_MAX_LEVELS];
	wf_carry carry_in;  // baseline inherited across exec, used at first listing
	wf_carry carry_out; // filled when this entry is closed
	int dead_drains;    // wf_reap calls since death; -1 once the symbolicator is freed
	int closed;         // final settlement done

	// Stacks of threads that ended before the energy for them was readable.
	// They are paid from the residual: what the kernel billed the process
	// beyond what its threads' counters ever showed. Entries are u64 words:
	// tid, t_ns, lr, flags, nframes, frames[nframes].
	uint64_t *orph;
	size_t orph_len;
	size_t orph_cap;
	uint32_t norph;
	uint32_t nbatch;                // leading entries being settled now
	uint32_t orph_age;              // ticks the current batch has waited
	uint64_t orph_min[2];           // smallest residual seen while it waited
	uint64_t resid_paid[2];         // residual already handed out: fastest level, the rest
	wf_cs_ref symbolicator; // built on first need, see wf_cs_ensure
	int sym_tried;
	uint64_t sym_refreshed;
	char name[256];
	char path[1024];
} wf_target;

struct wf_session {
	pthread_mutex_t mu; // targets, output buffer, stats
	wf_target **targets;
	int ntargets;
	int captargets;

	uint8_t *out;
	size_t out_len;
	size_t out_cap;
	uint64_t dropped;

	uint32_t interval_us;
	uint32_t max_depth;
	uint32_t defer_ticks; // how long energy may wait for a running stack
	uint32_t discover_ticks;
	uint32_t opaque_flush_ticks;
	uint64_t start_wall_us;
	int is_root;
	mach_timebase_info_data_t tb;
	uint64_t t0;
	uint64_t page_size;

	pthread_t sampler;
	int sampler_running;
	volatile int stop;
	volatile int sampling;

	pthread_t listener;
	int listener_running;
	volatile int listener_stop;
	mach_port_t rcv_port;
	char svc_name[128];

	pid_t root_pid;
	int root_is_child;
	int root_exited;
	int root_status;
	uint64_t root_tree_cpu_ns; // CPU time of the root and every descendant it waited for

	wf_stats stats;

	pthread_mutex_t sym_mu; // symbolicator handles

	// Sampler-thread scratch space.
	uint8_t *chunk;
	uint64_t chunk_addr;
	uint64_t chunk_len;
	uint64_t *frames;
	uint64_t tids[4096];
	int ntids;
	pid_t pids[1024];
};

// Build the target's symbolicator if that has not been attempted yet.
void wf_cs_ensure(struct wf_session *s, wf_target *t);

#endif
