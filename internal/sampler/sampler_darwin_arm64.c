// How wattflame attributes energy to code
//
// Apple Silicon kernels keep, for every thread, a running total of the energy
// its cores spent executing it (in nanojoules, split by performance level),
// next to cycles, instructions and CPU time. The totals are readable for any
// process you own through proc_pidinfo(PROC_PIDTHREADCOUNTS).
//
// The sampler wakes up every interval and, for each thread of the target:
//
//   1. reads the counters and accumulates the delta since the previous read;
//   2. if the thread is runnable, suspends it for a few microseconds and
//      walks its frame-pointer chain to get a stack;
//   3. once energy has accumulated and there are stacks to give it to, splits
//      everything accumulated evenly across those stacks.
//
// Step 3 exists because the kernel only folds energy into the per-thread total
// when the thread leaves a core or its quantum expires (about every 10 ms for
// a thread that never blocks), while stacks are taken every millisecond.
//
// A thread that works in bursts shorter than the interval is usually found
// blocked, with energy but no stack. Charging that energy to the place where
// it waits would make waiting look expensive. Instead the energy is held until
// a tick does catch the thread running, and goes to that stack: the same
// statistics a time profiler relies on, where a burst is seen in proportion to
// how long it lasts. Only if the thread is not caught for WF_DEFER_NS is a
// stack taken where it waits.
//
// Threads that do not run cost one counter read per tick and are never
// suspended.
//
// Which processes are covered
//
// Stacks need the process's task port. A process started under wattflame
// hands its own port over (see preload/preload.c); that is a "full" target.
// Processes that cannot do so (macOS system binaries, hardened apps, Intel
// binaries under Rosetta) are still found by walking the process tree, and
// their energy is still read from the per-thread counters, which need no task
// port. Those are "opaque" targets: counted, but without stacks. An opaque
// target becomes a full one the moment its handshake arrives.

#include "wf_internal.h"

#include <errno.h>
#include <libproc.h>
#include <mach/mach_time.h>
#include <mach/mach_vm.h>
#include <pthread/qos.h>
#include <servers/bootstrap.h>
#include <signal.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <sys/sysctl.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <unistd.h>

// Private proc_pidinfo flavors (xnu bsd/sys/proc_info_private.h).
#define WF_PROC_PIDLISTTHREADIDS 28
#define WF_PROC_PIDTHREADCOUNTS 34
#ifndef PROC_PIDTHREADID64INFO
#define PROC_PIDTHREADID64INFO 15
#endif

struct wf_ptc_data {
	uint64_t instructions;
	uint64_t cycles;
	uint64_t user_time_mach;
	uint64_t system_time_mach;
	uint64_t energy_nj;
};

struct wf_ptc {
	uint16_t len;
	uint16_t reserved0;
	uint32_t reserved1;
	struct wf_ptc_data counts[WF_MAX_LEVELS];
};

#define WF_MSG_HELLO 0x57464c4d
#define WF_MSG_GO 0x57464c47

#define WF_CHUNK 16384
#define WF_MAX_PENDING 2000
#define WF_DEFER_NS 50000000ULL       // how long energy may wait for a running stack
#define WF_DISCOVER_NS 10000000ULL    // how often the process tree is walked
#define WF_OPAQUE_FLUSH_NS 20000000ULL // how often opaque threads emit a record
#define WF_MAX_TARGETS 4096
#define WF_OUT_LIMIT ((size_t)1 << 30)
// User-space addresses fit in 47 bits; anything above is a pointer
// authentication code.
#define WF_ADDR_MASK 0x00007fffffffffffULL

extern kern_return_t bootstrap_register2(mach_port_t bp, name_t name, mach_port_t sp,
                                         uint64_t flags);

static void set_err(char *err, size_t errlen, const char *fmt, ...) {
	if (err == NULL || errlen == 0) {
		return;
	}
	va_list ap;
	va_start(ap, fmt);
	vsnprintf(err, errlen, fmt, ap);
	va_end(ap);
}

static uint64_t abs_to_ns(const wf_session *s, uint64_t abs) {
	return abs * s->tb.numer / s->tb.denom;
}

static uint64_t ns_to_abs(const wf_session *s, uint64_t ns) {
	return ns * s->tb.denom / s->tb.numer;
}

static uint32_t ticks_for(const wf_session *s, uint64_t ns) {
	uint64_t n = ns / ((uint64_t)s->interval_us * 1000);
	return n < 1 ? 1 : (uint32_t)n;
}

// ---------------------------------------------------------------------------
// Counters

static int read_counts(const wf_session *s, pid_t pid, uint64_t tid,
                       uint64_t out[WF_W_COUNT][WF_MAX_LEVELS]) {
	struct wf_ptc c;
	int r = proc_pidinfo(pid, WF_PROC_PIDTHREADCOUNTS, tid, &c, sizeof(c));
	if (r < (int)offsetof(struct wf_ptc, counts)) {
		return 0;
	}
	memset(out, 0, sizeof(uint64_t) * WF_W_COUNT * WF_MAX_LEVELS);
	int n = c.len < WF_MAX_LEVELS ? c.len : WF_MAX_LEVELS;
	for (int l = 0; l < n; l++) {
		out[WF_W_ENERGY_NJ][l] = c.counts[l].energy_nj;
		out[WF_W_CPU_NS][l] =
		    abs_to_ns(s, c.counts[l].user_time_mach + c.counts[l].system_time_mach);
		out[WF_W_CYCLES][l] = c.counts[l].cycles;
		out[WF_W_INSTR][l] = c.counts[l].instructions;
	}
	return 1;
}

static uint64_t process_energy_nj(pid_t pid) {
	struct rusage_info_v6 ri;
	if (proc_pid_rusage(pid, RUSAGE_INFO_V6, (rusage_info_t *)&ri) != 0) {
		return 0;
	}
	return ri.ri_energy_nj;
}

// Read the kernel's process total and remember how much thread energy had been
// seen at that same moment.
static void note_process_energy(wf_target *t) {
	uint64_t e = process_energy_nj(t->pid);
	if (e > t->energy_last) {
		t->energy_last = e;
		t->energy_seen_at_last = t->energy_seen;
	}
}

// Whether a process was started after the recording began. Such a process has
// spent no energy the recording should ignore, so its counters count from zero.
static int started_during_recording(const wf_session *s, pid_t pid) {
	struct proc_bsdinfo info;
	if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, sizeof(info)) < (int)sizeof(info)) {
		return 0;
	}
	uint64_t us = (uint64_t)info.pbi_start_tvsec * 1000000ULL + (uint64_t)info.pbi_start_tvusec;
	return us >= s->start_wall_us;
}

static uint64_t wall_us(void) {
	struct timeval tv;
	gettimeofday(&tv, NULL);
	return (uint64_t)tv.tv_sec * 1000000ULL + (uint64_t)tv.tv_usec;
}

// ---------------------------------------------------------------------------
// Output buffer

static void emit(wf_session *s, const wf_target *t, uint64_t tid, uint64_t t_ns, uint64_t lr,
                 uint32_t flags, const uint64_t *frames, uint32_t nframes,
                 uint64_t w[WF_W_COUNT][WF_MAX_LEVELS]) {
	size_t size = sizeof(wf_record) + (size_t)nframes * sizeof(uint64_t);
	if (s->out_len + size > s->out_cap) {
		size_t cap = s->out_cap ? s->out_cap * 2 : (1 << 20);
		while (cap < s->out_len + size) {
			cap *= 2;
		}
		if (cap > WF_OUT_LIMIT) {
			s->dropped++;
			return;
		}
		uint8_t *p = realloc(s->out, cap);
		if (p == NULL) {
			s->dropped++;
			return;
		}
		s->out = p;
		s->out_cap = cap;
	}
	wf_record *r = (wf_record *)(s->out + s->out_len);
	r->size = (uint32_t)size;
	r->target = t->index;
	r->pid = t->pid;
	r->nframes = nframes;
	r->tid = tid;
	r->t_ns = t_ns;
	r->lr = lr;
	r->flags = flags;
	r->reserved = 0;
	memcpy(r->w, w, sizeof(r->w));
	if (nframes > 0) {
		memcpy(r->frames, frames, (size_t)nframes * sizeof(uint64_t));
	}
	s->out_len += size;
}

// ---------------------------------------------------------------------------
// Stack walking

// Read the 16-byte frame record at fp. One read fetches everything from fp to
// the end of its page, which covers the next several frames in the common case.
static int read_frame(wf_session *s, task_t task, uint64_t fp, uint64_t rec[2]) {
	if (s->chunk_len >= 16 && fp >= s->chunk_addr && fp + 16 <= s->chunk_addr + s->chunk_len) {
		memcpy(rec, s->chunk + (fp - s->chunk_addr), 16);
		return 1;
	}
	uint64_t want = s->page_size - (fp & (s->page_size - 1));
	if (want < 16) {
		want = 16;
	}
	if (want > WF_CHUNK) {
		want = WF_CHUNK;
	}
	mach_vm_size_t got = 0;
	kern_return_t kr =
	    mach_vm_read_overwrite(task, fp, want, (mach_vm_address_t)s->chunk, &got);
	if (kr != KERN_SUCCESS || got < 16) {
		s->chunk_len = 0;
		return 0;
	}
	s->chunk_addr = fp;
	s->chunk_len = got;
	memcpy(rec, s->chunk, 16);
	return 1;
}

// Suspend the thread, copy its registers and walk the frame-pointer chain.
// Returns the number of frames, or -1 if the thread could not be inspected.
static int walk_stack(wf_session *s, wf_target *t, wf_thread *th, uint64_t *frames, uint32_t max,
                      uint64_t *lr, uint32_t *flags) {
	uint64_t t0 = mach_absolute_time();
	if (thread_suspend(th->port) != KERN_SUCCESS) {
		return -1;
	}

	int n = -1;
	arm_thread_state64_t st;
	mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
	if (thread_get_state(th->port, ARM_THREAD_STATE64, (thread_state_t)&st, &count) ==
	    KERN_SUCCESS) {
		uint64_t pc = (uint64_t)arm_thread_state64_get_pc(st) & WF_ADDR_MASK;
		uint64_t fp = (uint64_t)arm_thread_state64_get_fp(st);
		*lr = (uint64_t)arm_thread_state64_get_lr(st) & WF_ADDR_MASK;

		n = 0;
		frames[n++] = pc;
		s->chunk_len = 0;
		while (fp != 0 && (fp & 7) == 0) {
			if ((uint32_t)n >= max) {
				*flags |= WF_F_TRUNCATED;
				break;
			}
			uint64_t rec[2];
			if (!read_frame(s, t->task, fp, rec)) {
				break;
			}
			uint64_t ret = rec[1] & WF_ADDR_MASK;
			if (ret == 0) {
				break;
			}
			frames[n++] = ret;
			// Callers live at higher addresses. Anything else is a corrupt
			// or foreign frame record; stop rather than loop.
			if (rec[0] <= fp) {
				break;
			}
			fp = rec[0];
		}
	}

	thread_resume(th->port);
	uint64_t dt = abs_to_ns(s, mach_absolute_time() - t0);
	s->stats.suspend_ns += dt;
	if (dt > s->stats.suspend_max_ns) {
		s->stats.suspend_max_ns = dt;
	}
	return n;
}

// Keep a copy of a stack as the thread's last known one, for energy that turns
// up after the thread can no longer be sampled. Same word layout as a pending
// sample: t_ns, lr, flags, nframes, frames[nframes].
static void remember_stack(wf_thread *th, uint64_t now_ns, uint64_t lr, uint32_t flags,
                           const uint64_t *frames, int n) {
	size_t words = 4 + (size_t)n;
	if (words > th->last_cap) {
		uint64_t *p = realloc(th->last, words * sizeof(uint64_t));
		if (p == NULL) {
			th->last_len = 0;
			return;
		}
		th->last = p;
		th->last_cap = words;
	}
	th->last[0] = now_ns;
	th->last[1] = lr;
	th->last[2] = flags;
	th->last[3] = (uint64_t)n;
	memcpy(th->last + 4, frames, (size_t)n * sizeof(uint64_t));
	th->last_len = words;
}

static void take_sample(wf_session *s, wf_target *t, wf_thread *th, uint64_t now_ns,
                        uint32_t flags) {
	uint64_t lr = 0;
	int n = walk_stack(s, t, th, s->frames, s->max_depth, &lr, &flags);
	if (n < 0) {
		s->stats.sample_errors++;
		return;
	}
	s->stats.samples++;
	remember_stack(th, now_ns, lr, flags, s->frames, n);

	size_t need = th->pend_len + 4 + (size_t)n;
	if (need > th->pend_cap) {
		size_t cap = th->pend_cap ? th->pend_cap * 2 : 256;
		while (cap < need) {
			cap *= 2;
		}
		uint64_t *p = realloc(th->pend, cap * sizeof(uint64_t));
		if (p == NULL) {
			return;
		}
		th->pend = p;
		th->pend_cap = cap;
	}
	uint64_t *w = th->pend + th->pend_len;
	w[0] = now_ns;
	w[1] = lr;
	w[2] = flags;
	w[3] = (uint64_t)n;
	memcpy(w + 4, s->frames, (size_t)n * sizeof(uint64_t));
	th->pend_len = need;
	th->npend++;
}

static int acc_is_zero(const wf_thread *th) {
	for (int m = 0; m < WF_W_COUNT; m++) {
		for (int l = 0; l < WF_MAX_LEVELS; l++) {
			if (th->acc[m][l] != 0) {
				return 0;
			}
		}
	}
	return 1;
}

// Split what the thread accumulated evenly over its pending stacks and emit
// them. The division remainder goes to the last stack so totals stay exact.
static void flush_thread(wf_session *s, wf_target *t, wf_thread *th, uint64_t now_ns) {
	th->deferred = 0;
	if (th->npend == 0) {
		if (acc_is_zero(th)) {
			return;
		}
		if (th->last_len >= 4) {
			// No fresh stack (the thread is gone): use the last one seen.
			emit(s, t, th->tid, now_ns, th->last[1], (uint32_t)th->last[2] | WF_F_OFFCPU,
			     th->last + 4, (uint32_t)th->last[3], th->acc);
		} else {
			emit(s, t, th->tid, now_ns, 0, WF_F_NOSTACK | (t->opaque ? WF_F_OPAQUE : 0), NULL, 0,
			     th->acc);
		}
		memset(th->acc, 0, sizeof(th->acc));
		th->acc_energy = 0;
		return;
	}

	uint64_t share[WF_W_COUNT][WF_MAX_LEVELS];
	uint64_t last[WF_W_COUNT][WF_MAX_LEVELS];
	for (int m = 0; m < WF_W_COUNT; m++) {
		for (int l = 0; l < WF_MAX_LEVELS; l++) {
			share[m][l] = th->acc[m][l] / th->npend;
			last[m][l] = th->acc[m][l] - share[m][l] * (th->npend - 1);
		}
	}

	const uint64_t *w = th->pend;
	for (uint32_t i = 0; i < th->npend; i++) {
		uint32_t n = (uint32_t)w[3];
		emit(s, t, th->tid, w[0], w[1], (uint32_t)w[2], w + 4, n,
		     i + 1 == th->npend ? last : share);
		w += 4 + n;
	}
	th->npend = 0;
	th->pend_len = 0;
	memset(th->acc, 0, sizeof(th->acc));
	th->acc_energy = 0;
}

// ---------------------------------------------------------------------------
// Threads

static int thread_is_running(thread_act_t port) {
	thread_basic_info_data_t info;
	mach_msg_type_number_t count = THREAD_BASIC_INFO_COUNT;
	if (thread_info(port, THREAD_BASIC_INFO, (thread_info_t)&info, &count) != KERN_SUCCESS) {
		return 0;
	}
	return info.run_state == TH_STATE_RUNNING;
}

static void release_thread(wf_thread *th) {
	if (th->port != MACH_PORT_NULL) {
		mach_port_deallocate(mach_task_self(), th->port);
		th->port = MACH_PORT_NULL;
	}
	free(th->pend);
	th->pend = NULL;
	th->pend_len = th->pend_cap = 0;
	th->npend = 0;
	free(th->last);
	th->last = NULL;
	th->last_len = th->last_cap = 0;
}

// Flush and forget every thread of a target.
static void drop_threads(wf_session *s, wf_target *t, uint64_t now_ns) {
	for (int i = 0; i < t->nth; i++) {
		flush_thread(s, t, &t->th[i], now_ns);
		release_thread(&t->th[i]);
	}
	t->nth = 0;
}

static void target_died(wf_session *s, wf_target *t, uint64_t now_ns) {
	if (!t->alive) {
		return;
	}
	drop_threads(s, t, now_ns);
	t->alive = 0;
}

static wf_thread *new_thread(wf_target *t) {
	if (t->nth == t->capth) {
		int cap = t->capth ? t->capth * 2 : 16;
		wf_thread *p = realloc(t->th, (size_t)cap * sizeof(wf_thread));
		if (p == NULL) {
			return NULL;
		}
		t->th = p;
		t->capth = cap;
	}
	wf_thread *th = &t->th[t->nth++];
	memset(th, 0, sizeof(*th));
	th->mark = 1;
	return th;
}

static void sweep_threads(wf_session *s, wf_target *t, uint64_t now_ns) {
	for (int i = 0; i < t->nth;) {
		if (t->th[i].mark) {
			i++;
			continue;
		}
		flush_thread(s, t, &t->th[i], now_ns);
		release_thread(&t->th[i]);
		t->th[i] = t->th[t->nth - 1];
		t->nth--;
	}
	t->listed = 1;
}

// Where a newly found thread's counters start. A thread that existed before
// the recording starts from its current values. One born during the recording
// starts from zero, so the energy it used before it was noticed still counts.
static void set_baseline(wf_session *s, wf_target *t, wf_thread *th) {
	if (t->listed || t->from_zero) {
		th->have_prev = 1; // prev is all zero
	} else {
		th->have_prev = read_counts(s, t->pid, th->tid, th->prev);
	}
}

// Bring a full target's threads in line with its task.
static void refresh_threads(wf_session *s, wf_target *t, uint64_t now_ns) {
	thread_act_array_t list = NULL;
	mach_msg_type_number_t n = 0;
	if (task_threads(t->task, &list, &n) != KERN_SUCCESS) {
		target_died(s, t, now_ns);
		return;
	}

	for (int i = 0; i < t->nth; i++) {
		t->th[i].mark = 0;
	}
	for (mach_msg_type_number_t i = 0; i < n; i++) {
		wf_thread *found = NULL;
		for (int j = 0; j < t->nth; j++) {
			if (t->th[j].port == list[i]) {
				found = &t->th[j];
				break;
			}
		}
		if (found != NULL) {
			found->mark = 1;
			// task_threads handed us a second reference to a port we hold.
			mach_port_deallocate(mach_task_self(), list[i]);
			continue;
		}

		thread_identifier_info_data_t ident;
		mach_msg_type_number_t count = THREAD_IDENTIFIER_INFO_COUNT;
		wf_thread *th = NULL;
		if (thread_info(list[i], THREAD_IDENTIFIER_INFO, (thread_info_t)&ident, &count) ==
		    KERN_SUCCESS) {
			th = new_thread(t);
		}
		if (th == NULL) {
			mach_port_deallocate(mach_task_self(), list[i]);
			continue;
		}
		th->port = list[i];
		th->tid = ident.thread_id;
		set_baseline(s, t, th);
		if (t->listed) {
			// A thread that lives for less than a tick or two may never be
			// caught running. Note where it starts out so its energy still
			// has somewhere to go.
			uint64_t lr = 0;
			uint32_t flags = WF_F_OFFCPU;
			int depth = walk_stack(s, t, th, s->frames, s->max_depth, &lr, &flags);
			if (depth >= 0) {
				remember_stack(th, now_ns, lr, flags, s->frames, depth);
			}
		}
	}
	vm_deallocate(mach_task_self(), (vm_address_t)list, n * sizeof(thread_act_t));
	sweep_threads(s, t, now_ns);
}

// Bring an opaque target's threads in line with the kernel's thread id list
// (left in s->tids by thread_set_hash).
static void refresh_threads_opaque(wf_session *s, wf_target *t, uint64_t now_ns) {
	for (int i = 0; i < t->nth; i++) {
		t->th[i].mark = 0;
	}
	for (int i = 0; i < s->ntids; i++) {
		wf_thread *found = NULL;
		for (int j = 0; j < t->nth; j++) {
			if (t->th[j].tid == s->tids[i]) {
				found = &t->th[j];
				break;
			}
		}
		if (found != NULL) {
			found->mark = 1;
			continue;
		}
		wf_thread *th = new_thread(t);
		if (th == NULL) {
			continue;
		}
		th->tid = s->tids[i];
		set_baseline(s, t, th);
	}
	sweep_threads(s, t, now_ns);
}

// Cheap check for a changed thread set: a hash over the kernel's tid list.
// Returns 0 when the process no longer exists.
static uint64_t thread_set_hash(wf_session *s, pid_t pid) {
	int r = proc_pidinfo(pid, WF_PROC_PIDLISTTHREADIDS, 0, s->tids, sizeof(s->tids));
	if (r <= 0) {
		s->ntids = 0;
		return 0;
	}
	int n = r / (int)sizeof(uint64_t);
	s->ntids = n;
	uint64_t h = 1469598103934665603ULL ^ (uint64_t)n;
	for (int i = 0; i < n; i++) {
		h = (h ^ s->tids[i]) * 1099511628211ULL;
	}
	return h | 1;
}

static void tick_thread(wf_session *s, wf_target *t, wf_thread *th, uint64_t now_ns) {
	uint64_t cur[WF_W_COUNT][WF_MAX_LEVELS];
	if (!read_counts(s, t->pid, th->tid, cur)) {
		// Thread is gone; whatever it still owes goes to its last stacks.
		flush_thread(s, t, th, now_ns);
		return;
	}

	if (th->have_prev) {
		for (int m = 0; m < WF_W_COUNT; m++) {
			for (int l = 0; l < WF_MAX_LEVELS; l++) {
				if (cur[m][l] > th->prev[m][l]) {
					uint64_t d = cur[m][l] - th->prev[m][l];
					th->acc[m][l] += d;
					if (m == WF_W_ENERGY_NJ) {
						th->acc_energy += d;
						t->energy_seen += d;
					}
				}
			}
		}
	}
	memcpy(th->prev, cur, sizeof(cur));
	th->have_prev = 1;

	if (t->opaque) {
		// No stacks to wait for; just keep the record count down.
		if (th->acc_energy > 0 && ++th->deferred >= s->opaque_flush_ticks) {
			flush_thread(s, t, th, now_ns);
		}
		return;
	}

	if (thread_is_running(th->port)) {
		take_sample(s, t, th, now_ns, 0);
	}
	if (th->acc_energy > 0) {
		if (th->npend > 0) {
			flush_thread(s, t, th, now_ns);
		} else if (++th->deferred >= s->defer_ticks) {
			// Not caught running for a while: settle for where it waits.
			take_sample(s, t, th, now_ns, WF_F_OFFCPU);
			flush_thread(s, t, th, now_ns);
		}
	} else if (th->npend >= WF_MAX_PENDING) {
		flush_thread(s, t, th, now_ns);
	}
}

// ---------------------------------------------------------------------------
// Targets

static wf_target *find_alive(wf_session *s, pid_t pid) {
	for (int i = 0; i < s->ntargets; i++) {
		if (s->targets[i]->alive && s->targets[i]->pid == pid) {
			return s->targets[i];
		}
	}
	return NULL;
}

// Append a target. The caller holds s->mu and owns task (may be null).
static wf_target *new_target(wf_session *s, pid_t pid, task_t task, int from_zero) {
	if (s->ntargets >= WF_MAX_TARGETS) {
		return NULL;
	}
	wf_target *t = calloc(1, sizeof(*t));
	if (t == NULL) {
		return NULL;
	}
	t->index = (uint32_t)s->ntargets;
	t->pid = pid;
	t->task = task;
	t->opaque = task == MACH_PORT_NULL;
	t->alive = 1;
	t->from_zero = from_zero;
	t->energy_start = from_zero ? 0 : process_energy_nj(pid);
	t->energy_last = t->energy_start;
	if (proc_name(pid, t->name, sizeof(t->name)) <= 0) {
		snprintf(t->name, sizeof(t->name), "pid %d", pid);
	}
	if (proc_pidpath(pid, t->path, sizeof(t->path)) <= 0) {
		t->path[0] = '\0';
	}
	s->targets[s->ntargets++] = t;
	return t;
}

// Track a process by pid alone: energy, no stacks. Caller holds s->mu.
static void add_opaque(wf_session *s, pid_t pid, int from_zero) {
	new_target(s, pid, MACH_PORT_NULL, from_zero);
}

static void send_go(mach_port_t reply) {
	if (reply == MACH_PORT_NULL) {
		return;
	}
	mach_msg_header_t h;
	memset(&h, 0, sizeof(h));
	h.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MOVE_SEND_ONCE, 0);
	h.msgh_size = sizeof(h);
	h.msgh_remote_port = reply;
	h.msgh_id = WF_MSG_GO;
	if (mach_msg(&h, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof(h), 0, MACH_PORT_NULL, 1000,
	             MACH_PORT_NULL) != KERN_SUCCESS) {
		mach_msg_destroy(&h);
	}
}

// Register a task whose port we now hold. Takes ownership of the task send
// right and of the reply port. If sampling has not started yet the reply is
// held back, which keeps the process parked in its handshake until wf_start.
static void add_target(wf_session *s, pid_t pid, task_t task, mach_port_t reply) {
	uint64_t now_ns = abs_to_ns(s, mach_absolute_time() - s->t0);

	pthread_mutex_lock(&s->mu);
	wf_target *t = NULL;
	for (int i = 0; i < s->ntargets && t == NULL; i++) {
		wf_target *old = s->targets[i];
		if (!old->alive) {
			continue;
		}
		if (!old->opaque && old->task == task) {
			// Same task announced twice (attached by task_for_pid, then its
			// own handshake). Nothing to add.
			mach_port_deallocate(mach_task_self(), task);
			pthread_mutex_unlock(&s->mu);
			send_go(reply);
			return;
		}
		if (old->pid != pid) {
			continue;
		}
		char path[sizeof(old->path)];
		if (old->opaque && proc_pidpath(pid, path, sizeof(path)) > 0 &&
		    strcmp(path, old->path) == 0) {
			// The process was already being counted by pid; now it can be
			// sampled too. What it used until now stays on record without
			// a stack.
			drop_threads(s, old, now_ns);
			pthread_mutex_lock(&s->sym_mu);
			old->task = task;
			pthread_mutex_unlock(&s->sym_mu);
			old->opaque = 0;
			old->listed = 0;
			old->from_zero = 0;
			t = old;
		} else {
			// Same pid but a new task or a new executable: the process
			// called exec.
			target_died(s, old, now_ns);
		}
	}
	if (t == NULL) {
		t = new_target(s, pid, task, 0);
	}
	if (t == NULL) {
		mach_port_deallocate(mach_task_self(), task);
		pthread_mutex_unlock(&s->mu);
		send_go(reply);
		return;
	}
	refresh_threads(s, t, now_ns);
	t->thread_hash = thread_set_hash(s, pid);

	if (s->sampling) {
		pthread_mutex_unlock(&s->mu);
		send_go(reply);
	} else {
		t->pending_reply = reply;
		pthread_mutex_unlock(&s->mu);
	}
	// Reading the task's image list takes a millisecond or so. The process
	// has been released by now and does not wait for it.
	wf_cs_ensure(s, t);
}

// Find child processes of everything being tracked. Most announce themselves
// through the handshake; this catches the ones that cannot.
static void discover_children(wf_session *s) {
	int n = s->ntargets;
	for (int i = 0; i < n; i++) {
		wf_target *t = s->targets[i];
		if (!t->alive) {
			continue;
		}
		int bytes = proc_listpids(PROC_PPID_ONLY, (uint32_t)t->pid, s->pids, (int)sizeof(s->pids));
		if (bytes <= 0) {
			continue;
		}
		int count = bytes / (int)sizeof(pid_t);
		for (int k = 0; k < count; k++) {
			pid_t pid = s->pids[k];
			if (pid <= 0 || pid == getpid() || find_alive(s, pid) != NULL) {
				continue;
			}
			int from_zero = started_during_recording(s, pid);
			// Root may take the task port of any process that is not a
			// protected system binary, handshake or not.
			task_t task = MACH_PORT_NULL;
			if (s->is_root && task_for_pid(mach_task_self(), pid, &task) == KERN_SUCCESS) {
				if (new_target(s, pid, task, from_zero) == NULL) {
					mach_port_deallocate(mach_task_self(), task);
				}
				continue;
			}
			add_opaque(s, pid, from_zero);
		}
	}
}

static void tick_target(wf_session *s, wf_target *t, uint64_t now_ns) {
	if (!t->alive) {
		return;
	}
	uint64_t h = thread_set_hash(s, t->pid);
	if (h == 0) {
		// The pid no longer answers: the process has exited.
		target_died(s, t, now_ns);
		return;
	}
	if (h != t->thread_hash || !t->listed) {
		if (t->opaque) {
			// A different executable under the same pid means exec: close
			// this entry and start a new one under the new name. exec
			// carries the thread's counters over, so the new entry starts
			// from their current values, not from zero.
			char path[sizeof(t->path)];
			if (t->listed && proc_pidpath(t->pid, path, sizeof(path)) > 0 &&
			    strcmp(path, t->path) != 0) {
				target_died(s, t, now_ns);
				add_opaque(s, t->pid, 0);
				return;
			}
			refresh_threads_opaque(s, t, now_ns);
		} else {
			refresh_threads(s, t, now_ns);
			if (!t->alive) {
				// The task is gone but the pid is not: exec into a program
				// that does not announce itself.
				if (find_alive(s, t->pid) == NULL) {
					add_opaque(s, t->pid, 0);
				}
				return;
			}
		}
		t->thread_hash = h;
	}
	for (int i = 0; i < t->nth; i++) {
		tick_thread(s, t, &t->th[i], now_ns);
	}
	if (s->stats.ticks % 8 == 0) {
		note_process_energy(t);
	}
}

// Notice the root process ending.
static void check_root(wf_session *s, uint64_t now_ns) {
	if (s->root_exited) {
		return;
	}
	if (s->root_is_child) {
		// WNOWAIT leaves the zombie in place until its targets are closed.
		siginfo_t info;
		memset(&info, 0, sizeof(info));
		if (waitid(P_PID, (id_t)s->root_pid, &info, WEXITED | WNOHANG | WNOWAIT) != 0 ||
		    info.si_pid != s->root_pid) {
			return;
		}
		wf_target *t = find_alive(s, s->root_pid);
		if (t != NULL) {
			target_died(s, t, now_ns);
		}
		int status = 0;
		waitpid(s->root_pid, &status, 0);
		s->root_status = status;
		s->root_exited = 1;
		return;
	}
	if (find_alive(s, s->root_pid) == NULL) {
		s->root_exited = 1;
	}
}

static void *sampler_main(void *arg) {
	wf_session *s = arg;
	pthread_setname_np("wattflame-sampler");
	pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0);

	uint64_t self_tid = 0;
	pthread_threadid_np(NULL, &self_tid);
	uint64_t self0[WF_W_COUNT][WF_MAX_LEVELS];
	int have_self = read_counts(s, getpid(), self_tid, self0);

	uint64_t interval = ns_to_abs(s, (uint64_t)s->interval_us * 1000);
	if (interval == 0) {
		interval = 1;
	}
	uint64_t next = mach_absolute_time() + interval;

	for (;;) {
		mach_wait_until(next);
		if (s->stop) {
			break;
		}
		uint64_t now = mach_absolute_time();
		uint64_t now_ns = abs_to_ns(s, now - s->t0);

		pthread_mutex_lock(&s->mu);
		if (s->stats.ticks % s->discover_ticks == 0) {
			discover_children(s);
		}
		// Targets added during the loop are picked up by the same pass.
		for (int i = 0; i < s->ntargets; i++) {
			tick_target(s, s->targets[i], now_ns);
		}
		check_root(s, now_ns);
		s->stats.ticks++;
		int done = s->root_exited;
		pthread_mutex_unlock(&s->mu);
		if (done) {
			break;
		}

		next += interval;
		uint64_t after = mach_absolute_time();
		if (next <= after) {
			uint64_t skipped = (after - next) / interval + 1;
			s->stats.overruns += skipped;
			next += skipped * interval;
		}
	}

	// Final flush: pick up the last counter movement of every live thread.
	uint64_t end = mach_absolute_time();
	uint64_t end_ns = abs_to_ns(s, end - s->t0);
	pthread_mutex_lock(&s->mu);
	for (int i = 0; i < s->ntargets; i++) {
		wf_target *t = s->targets[i];
		if (!t->alive) {
			continue;
		}
		for (int j = 0; j < t->nth; j++) {
			wf_thread *th = &t->th[j];
			uint64_t cur[WF_W_COUNT][WF_MAX_LEVELS];
			if (th->have_prev && read_counts(s, t->pid, th->tid, cur)) {
				for (int m = 0; m < WF_W_COUNT; m++) {
					for (int l = 0; l < WF_MAX_LEVELS; l++) {
						if (cur[m][l] > th->prev[m][l]) {
							uint64_t d = cur[m][l] - th->prev[m][l];
							th->acc[m][l] += d;
							if (m == WF_W_ENERGY_NJ) {
								th->acc_energy += d;
								t->energy_seen += d;
							}
						}
					}
				}
				memcpy(th->prev, cur, sizeof(cur));
			}
			if (!t->opaque && th->npend == 0 && !acc_is_zero(th)) {
				take_sample(s, t, th, end_ns, WF_F_OFFCPU);
			}
			flush_thread(s, t, th, end_ns);
		}
		note_process_energy(t);
	}
	s->stats.elapsed_ns = end_ns;
	pthread_mutex_unlock(&s->mu);

	uint64_t self1[WF_W_COUNT][WF_MAX_LEVELS];
	if (have_self && read_counts(s, getpid(), self_tid, self1)) {
		for (int l = 0; l < WF_MAX_LEVELS; l++) {
			s->stats.self_energy_nj += self1[WF_W_ENERGY_NJ][l] - self0[WF_W_ENERGY_NJ][l];
			s->stats.self_cpu_ns += self1[WF_W_CPU_NS][l] - self0[WF_W_CPU_NS][l];
		}
	}
	return NULL;
}

// ---------------------------------------------------------------------------
// Handshake listener

typedef struct {
	mach_msg_header_t hdr;
	mach_msg_body_t body;
	mach_msg_port_descriptor_t task;
	int32_t pid;
	uint8_t trailer[128];
} wf_hello_rcv;

static void *listener_main(void *arg) {
	wf_session *s = arg;
	pthread_setname_np("wattflame-listener");
	while (!s->listener_stop) {
		wf_hello_rcv m;
		memset(&m, 0, sizeof(m));
		kern_return_t kr = mach_msg(&m.hdr, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof(m),
		                            s->rcv_port, 100, MACH_PORT_NULL);
		if (kr == MACH_RCV_TIMED_OUT) {
			continue;
		}
		if (kr != KERN_SUCCESS) {
			usleep(1000);
			continue;
		}
		if (m.hdr.msgh_id != WF_MSG_HELLO || !(m.hdr.msgh_bits & MACH_MSGH_BITS_COMPLEX) ||
		    m.body.msgh_descriptor_count != 1 || m.task.type != MACH_MSG_PORT_DESCRIPTOR) {
			mach_msg_destroy(&m.hdr);
			continue;
		}
		task_t task = m.task.name;
		mach_port_t reply = m.hdr.msgh_remote_port;
		// Do not trust the pid in the message; ask the kernel.
		int pid = 0;
		if (task == MACH_PORT_NULL || pid_for_task(task, &pid) != KERN_SUCCESS || pid <= 0) {
			mach_msg_destroy(&m.hdr);
			continue;
		}
		add_target(s, pid, task, reply);
	}
	return NULL;
}

static int start_listener(wf_session *s, char *err, size_t errlen) {
	if (mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &s->rcv_port) !=
	    KERN_SUCCESS) {
		set_err(err, errlen, "cannot allocate a Mach port");
		return -1;
	}
	mach_port_insert_right(mach_task_self(), s->rcv_port, s->rcv_port, MACH_MSG_TYPE_MAKE_SEND);
	snprintf(s->svc_name, sizeof(s->svc_name), "wattflame.%d.%llx", getpid(),
	         (unsigned long long)wall_us());
	kern_return_t kr = bootstrap_register2(bootstrap_port, s->svc_name, s->rcv_port, 0);
	if (kr != KERN_SUCCESS) {
		set_err(err, errlen, "cannot register the handshake service (bootstrap error %d)", kr);
		return -1;
	}
	if (pthread_create(&s->listener, NULL, listener_main, s) != 0) {
		set_err(err, errlen, "cannot start the listener thread");
		return -1;
	}
	s->listener_running = 1;
	return 0;
}

// ---------------------------------------------------------------------------
// Public API

wf_session *wf_new(char *err, size_t errlen) {
	wf_session *s = calloc(1, sizeof(*s));
	if (s == NULL) {
		set_err(err, errlen, "out of memory");
		return NULL;
	}
	pthread_mutex_init(&s->mu, NULL);
	pthread_mutex_init(&s->sym_mu, NULL);
	mach_timebase_info(&s->tb);
	s->t0 = mach_absolute_time();
	s->start_wall_us = wall_us();
	s->page_size = (uint64_t)vm_page_size;
	s->is_root = geteuid() == 0;
	s->interval_us = 1000;
	s->targets = calloc(WF_MAX_TARGETS, sizeof(wf_target *));
	s->chunk = malloc(WF_CHUNK);
	s->frames = malloc(WF_MAX_DEPTH * sizeof(uint64_t));
	if (s->targets == NULL || s->chunk == NULL || s->frames == NULL) {
		set_err(err, errlen, "out of memory");
		wf_free(s);
		return NULL;
	}
	int n = 0;
	size_t len = sizeof(n);
	if (sysctlbyname("hw.nperflevels", &n, &len, NULL, 0) != 0 || n < 1) {
		n = 1;
	}
	s->stats.nlevels = n > WF_MAX_LEVELS ? WF_MAX_LEVELS : (uint32_t)n;
	return s;
}

int wf_perf_levels(char names[WF_MAX_LEVELS][32]) {
	int n = 0;
	size_t len = sizeof(n);
	if (sysctlbyname("hw.nperflevels", &n, &len, NULL, 0) != 0 || n < 1) {
		n = 1;
	}
	if (n > WF_MAX_LEVELS) {
		n = WF_MAX_LEVELS;
	}
	for (int i = 0; i < n; i++) {
		char key[64];
		snprintf(key, sizeof(key), "hw.perflevel%d.name", i);
		len = 32;
		names[i][0] = '\0';
		if (sysctlbyname(key, names[i], &len, NULL, 0) != 0 || names[i][0] == '\0') {
			snprintf(names[i], 32, "Level %d", i);
		}
		names[i][31] = '\0';
	}
	return n;
}

static int has_prefix(const char *s, const char *prefix) {
	return strncmp(s, prefix, strlen(prefix)) == 0;
}

int wf_launch(wf_session *s, const char *preload_path, char *const argv[], char *const envp[],
              char *err, size_t errlen) {
	if (s->root_pid != 0) {
		set_err(err, errlen, "session already has a target");
		return -1;
	}
	if (start_listener(s, err, errlen) != 0) {
		return -1;
	}

	// Child environment: the caller's, minus our two variables, plus fresh ones.
	size_t n = 0;
	while (envp[n] != NULL) {
		n++;
	}
	char **env = calloc(n + 3, sizeof(char *));
	char port_var[192];
	char *insert_var = NULL;
	const char *existing = NULL;
	if (env == NULL) {
		set_err(err, errlen, "out of memory");
		return -1;
	}
	size_t k = 0;
	for (size_t i = 0; i < n; i++) {
		if (has_prefix(envp[i], "WATTFLAME_PORT=")) {
			continue;
		}
		if (has_prefix(envp[i], "DYLD_INSERT_LIBRARIES=")) {
			existing = envp[i] + strlen("DYLD_INSERT_LIBRARIES=");
			continue;
		}
		env[k++] = envp[i];
	}
	snprintf(port_var, sizeof(port_var), "WATTFLAME_PORT=%s", s->svc_name);
	env[k++] = port_var;
	if (existing != NULL && existing[0] != '\0') {
		asprintf(&insert_var, "DYLD_INSERT_LIBRARIES=%s:%s", preload_path, existing);
	} else {
		asprintf(&insert_var, "DYLD_INSERT_LIBRARIES=%s", preload_path);
	}
	if (insert_var == NULL) {
		free(env);
		set_err(err, errlen, "out of memory");
		return -1;
	}
	env[k++] = insert_var;
	env[k] = NULL;

	// The program starts suspended and is only let go by wf_start, once the
	// sampler is running, so nothing it does goes unobserved.
	posix_spawnattr_t attr;
	posix_spawnattr_init(&attr);
	posix_spawnattr_setflags(&attr, POSIX_SPAWN_START_SUSPENDED);
	pid_t pid = 0;
	int rc = posix_spawnp(&pid, argv[0], NULL, &attr, argv, env);
	posix_spawnattr_destroy(&attr);
	free(insert_var);
	free(env);
	if (rc != 0) {
		set_err(err, errlen, "cannot run %s: %s", argv[0], strerror(rc));
		return -1;
	}

	task_t task = MACH_PORT_NULL;
	int have_task = s->is_root && task_for_pid(mach_task_self(), pid, &task) == KERN_SUCCESS;

	pthread_mutex_lock(&s->mu);
	s->root_pid = pid;
	s->root_is_child = 1;
	s->root_suspended = 1;
	if (!have_task) {
		// Count it by pid from the start. If it can be sampled, its
		// handshake will arrive before main() and upgrade this entry.
		add_opaque(s, pid, 1);
	}
	pthread_mutex_unlock(&s->mu);
	if (have_task) {
		add_target(s, pid, task, MACH_PORT_NULL);
	}
	return 0;
}

int wf_attach(wf_session *s, pid_t pid, char *err, size_t errlen) {
	if (s->root_pid != 0) {
		set_err(err, errlen, "session already has a target");
		return -1;
	}
	if (pid == getpid()) {
		set_err(err, errlen, "cannot profile wattflame itself");
		return -1;
	}
	if (kill(pid, 0) != 0 && errno == ESRCH) {
		set_err(err, errlen, "no process with pid %d", pid);
		return -1;
	}
	task_t task = MACH_PORT_NULL;
	kern_return_t kr = task_for_pid(mach_task_self(), pid, &task);
	if (kr != KERN_SUCCESS) {
		if (!s->is_root) {
			set_err(err, errlen,
			        "cannot get the task port of pid %d. Attaching to a running process "
			        "needs root: run `sudo wattflame record -p %d`. (Launching the program "
			        "through `wattflame record -- <command>` does not need sudo.)",
			        pid, pid);
		} else {
			set_err(err, errlen,
			        "cannot get the task port of pid %d even as root. macOS system "
			        "processes and apps signed with the hardened runtime are protected "
			        "by System Integrity Protection and cannot be sampled.",
			        pid);
		}
		return -1;
	}
	pthread_mutex_lock(&s->mu);
	s->root_pid = pid;
	s->root_is_child = 0;
	pthread_mutex_unlock(&s->mu);
	add_target(s, pid, task, MACH_PORT_NULL);
	return 0;
}

int wf_start(wf_session *s, uint32_t interval_us, uint32_t max_depth, char *err, size_t errlen) {
	if (s->sampling) {
		set_err(err, errlen, "already sampling");
		return -1;
	}
	if (s->ntargets == 0) {
		set_err(err, errlen, "no target");
		return -1;
	}
	if (interval_us < 100) {
		interval_us = 100;
	}
	if (max_depth < 1) {
		max_depth = 1;
	}
	if (max_depth > WF_MAX_DEPTH) {
		max_depth = WF_MAX_DEPTH;
	}
	s->interval_us = interval_us;
	s->max_depth = max_depth;
	s->defer_ticks = ticks_for(s, WF_DEFER_NS);
	if (s->defer_ticks < 4) {
		s->defer_ticks = 4;
	}
	s->discover_ticks = ticks_for(s, WF_DISCOVER_NS);
	s->opaque_flush_ticks = ticks_for(s, WF_OPAQUE_FLUSH_NS);

	pthread_mutex_lock(&s->mu);
	s->t0 = mach_absolute_time();
	s->stop = 0;
	s->sampling = 1;
	// Let every parked process go. Baselines were taken when each was added,
	// while it sat blocked, so nothing it does from here on is missed.
	for (int i = 0; i < s->ntargets; i++) {
		mach_port_t reply = s->targets[i]->pending_reply;
		s->targets[i]->pending_reply = MACH_PORT_NULL;
		send_go(reply);
	}
	pthread_mutex_unlock(&s->mu);

	if (pthread_create(&s->sampler, NULL, sampler_main, s) != 0) {
		s->sampling = 0;
		set_err(err, errlen, "cannot start the sampler thread");
		return -1;
	}
	s->sampler_running = 1;
	if (s->root_suspended) {
		kill(s->root_pid, SIGCONT);
		s->root_suspended = 0;
	}
	return 0;
}

void wf_stop(wf_session *s) {
	if (!s->sampler_running) {
		return;
	}
	s->stop = 1;
	pthread_join(s->sampler, NULL);
	s->sampler_running = 0;
}

size_t wf_drain(wf_session *s, void *buf, size_t cap) {
	pthread_mutex_lock(&s->mu);
	// Hand out whole records only.
	size_t n = 0;
	while (n < s->out_len) {
		const wf_record *r = (const wf_record *)(s->out + n);
		if (n + r->size > cap) {
			break;
		}
		n += r->size;
	}
	if (n > 0) {
		memcpy(buf, s->out, n);
		memmove(s->out, s->out + n, s->out_len - n);
		s->out_len -= n;
	}
	pthread_mutex_unlock(&s->mu);
	return n;
}

int wf_root_exited(wf_session *s, int *status) {
	pthread_mutex_lock(&s->mu);
	int e = s->root_exited;
	if (status != NULL) {
		*status = s->root_status;
	}
	pthread_mutex_unlock(&s->mu);
	return e;
}

pid_t wf_root_pid(wf_session *s) {
	return s->root_pid;
}

void wf_kill_root(wf_session *s, int sig) {
	pthread_mutex_lock(&s->mu);
	if (s->root_pid > 0 && !s->root_exited) {
		kill(s->root_pid, sig);
	}
	pthread_mutex_unlock(&s->mu);
}

void wf_get_stats(wf_session *s, wf_stats *out) {
	pthread_mutex_lock(&s->mu);
	*out = s->stats;
	out->targets = (uint32_t)s->ntargets;
	out->dropped = s->dropped;
	if (out->elapsed_ns == 0) {
		out->elapsed_ns = abs_to_ns(s, mach_absolute_time() - s->t0);
	}
	pthread_mutex_unlock(&s->mu);
}

int wf_target_count(wf_session *s) {
	pthread_mutex_lock(&s->mu);
	int n = s->ntargets;
	pthread_mutex_unlock(&s->mu);
	return n;
}

int wf_get_target(wf_session *s, uint32_t target, wf_target_info *out) {
	memset(out, 0, sizeof(*out));
	pthread_mutex_lock(&s->mu);
	if (target >= (uint32_t)s->ntargets) {
		pthread_mutex_unlock(&s->mu);
		return 0;
	}
	const wf_target *t = s->targets[target];
	out->pid = t->pid;
	out->alive = t->alive;
	out->opaque = t->opaque;
	out->energy_nj = t->energy_last > t->energy_start ? t->energy_last - t->energy_start : 0;
	out->threads_energy_nj = t->energy_seen_at_last;
	strlcpy(out->name, t->name, sizeof(out->name));
	strlcpy(out->path, t->path, sizeof(out->path));
	pthread_mutex_unlock(&s->mu);
	return 1;
}

void wf_thread_name(int32_t pid, uint64_t tid, char *out, size_t outlen) {
	if (outlen == 0) {
		return;
	}
	out[0] = '\0';
	struct proc_threadinfo info;
	int r = proc_pidinfo(pid, PROC_PIDTHREADID64INFO, tid, &info, sizeof(info));
	if (r >= (int)sizeof(info)) {
		info.pth_name[sizeof(info.pth_name) - 1] = '\0';
		strlcpy(out, info.pth_name, outlen);
	}
}

void wf_free(wf_session *s) {
	if (s == NULL) {
		return;
	}
	wf_stop(s);
	if (s->listener_running) {
		s->listener_stop = 1;
		pthread_join(s->listener, NULL);
		s->listener_running = 0;
	}
	if (s->root_is_child && !s->root_exited && s->root_pid > 0) {
		// Never leave a child parked in its handshake or suspended. Ask
		// politely first so it can clean up, then insist.
		int status = 0;
		kill(s->root_pid, SIGTERM);
		kill(s->root_pid, SIGCONT);
		for (int i = 0; i < 100; i++) {
			if (waitpid(s->root_pid, &status, WNOHANG) == s->root_pid) {
				s->root_exited = 1;
				break;
			}
			usleep(10000);
		}
		if (!s->root_exited) {
			kill(s->root_pid, SIGKILL);
			waitpid(s->root_pid, &status, 0);
			s->root_exited = 1;
		}
	}
	if (s->targets != NULL) {
		for (int i = 0; i < s->ntargets; i++) {
			wf_target *t = s->targets[i];
			send_go(t->pending_reply);
			for (int j = 0; j < t->nth; j++) {
				release_thread(&t->th[j]);
			}
			free(t->th);
			wf_cs_release(t->symbolicator);
			if (t->task != MACH_PORT_NULL) {
				mach_port_deallocate(mach_task_self(), t->task);
			}
			free(t);
		}
		free(s->targets);
	}
	if (s->rcv_port != MACH_PORT_NULL) {
		mach_port_mod_refs(mach_task_self(), s->rcv_port, MACH_PORT_RIGHT_RECEIVE, -1);
		mach_port_deallocate(mach_task_self(), s->rcv_port);
	}
	free(s->out);
	free(s->chunk);
	free(s->frames);
	pthread_mutex_destroy(&s->mu);
	pthread_mutex_destroy(&s->sym_mu);
	free(s);
}
