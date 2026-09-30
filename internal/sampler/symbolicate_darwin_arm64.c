// Address -> function name, using CoreSymbolication, the private framework
// behind Apple's own `sample` and `atos`. It understands the dyld shared cache,
// dSYMs and debug maps, and keeps answering after the target has exited.
//
// It is loaded with dlopen so that a future macOS without it degrades to
// unsymbolicated addresses instead of a binary that does not start.

#include "wf_internal.h"

#include <dlfcn.h>
#include <mach/mach_time.h>
#include <string.h>

typedef struct {
	uint64_t location;
	uint64_t length;
} wf_cs_range;

#define WF_CS_NOW 0x8000000000000000ULL

static struct {
	int ok;
	wf_cs_ref (*create_with_task)(task_t);
	wf_cs_ref (*symbol_at)(wf_cs_ref, mach_vm_address_t, uint64_t);
	wf_cs_ref (*owner_at)(wf_cs_ref, mach_vm_address_t, uint64_t);
	wf_cs_ref (*source_at)(wf_cs_ref, mach_vm_address_t, uint64_t);
	const char *(*symbol_name)(wf_cs_ref);
	wf_cs_range (*symbol_range)(wf_cs_ref);
	const char *(*owner_name)(wf_cs_ref);
	const char *(*owner_path)(wf_cs_ref);
	const char *(*source_path)(wf_cs_ref);
	uint32_t (*source_line)(wf_cs_ref);
	int (*is_null)(wf_cs_ref);
	void (*release)(wf_cs_ref);
} cs;

static pthread_once_t cs_once = PTHREAD_ONCE_INIT;

// The dyld shared cache is mapped at the same address in every process, so
// our own mapping tells us where the target's is.
extern const void *_dyld_get_shared_cache_range(size_t *length);

static int in_shared_cache(uint64_t addr) {
	static uint64_t start, end;
	static int known;
	if (!known) {
		size_t len = 0;
		const void *base = _dyld_get_shared_cache_range(&len);
		start = (uint64_t)(uintptr_t)base;
		end = start + len;
		known = 1;
	}
	return start != 0 && addr >= start && addr < end;
}

static void cs_load(void) {
	void *h = dlopen(
	    "/System/Library/PrivateFrameworks/CoreSymbolication.framework/CoreSymbolication",
	    RTLD_NOW | RTLD_LOCAL);
	if (h == NULL) {
		return;
	}
	cs.create_with_task = dlsym(h, "CSSymbolicatorCreateWithTask");
	cs.symbol_at = dlsym(h, "CSSymbolicatorGetSymbolWithAddressAtTime");
	cs.owner_at = dlsym(h, "CSSymbolicatorGetSymbolOwnerWithAddressAtTime");
	cs.source_at = dlsym(h, "CSSymbolicatorGetSourceInfoWithAddressAtTime");
	cs.symbol_name = dlsym(h, "CSSymbolGetName");
	cs.symbol_range = dlsym(h, "CSSymbolGetRange");
	cs.owner_name = dlsym(h, "CSSymbolOwnerGetName");
	cs.owner_path = dlsym(h, "CSSymbolOwnerGetPath");
	cs.source_path = dlsym(h, "CSSourceInfoGetPath");
	cs.source_line = dlsym(h, "CSSourceInfoGetLineNumber");
	cs.is_null = dlsym(h, "CSIsNull");
	cs.release = dlsym(h, "CSRelease");
	cs.ok = cs.create_with_task && cs.symbol_at && cs.owner_at && cs.symbol_name &&
	        cs.symbol_range && cs.owner_name && cs.is_null && cs.release;
}

int wf_cs_is_null(wf_cs_ref ref) {
	return ref.data == NULL && ref.obj == NULL;
}

wf_cs_ref wf_cs_create(task_t task) {
	wf_cs_ref null_ref = {NULL, NULL};
	pthread_once(&cs_once, cs_load);
	if (!cs.ok) {
		return null_ref;
	}
	wf_cs_ref ref = cs.create_with_task(task);
	if (cs.is_null(ref)) {
		return null_ref;
	}
	return ref;
}

void wf_cs_release(wf_cs_ref ref) {
	if (cs.ok && !wf_cs_is_null(ref)) {
		cs.release(ref);
	}
}

static void ensure_locked(wf_target *t) {
	if (t->sym_tried || t->task == MACH_PORT_NULL) {
		return;
	}
	t->sym_tried = 1;
	t->symbolicator = wf_cs_create(t->task);
	t->sym_refreshed = mach_absolute_time();
}

void wf_cs_ensure(wf_session *s, wf_target *t) {
	pthread_mutex_lock(&s->sym_mu);
	ensure_locked(t);
	pthread_mutex_unlock(&s->sym_mu);
}

int wf_symbolicate(wf_session *s, uint32_t target, uint64_t addr, wf_symbol *out) {
	memset(out, 0, sizeof(*out));
	pthread_once(&cs_once, cs_load);
	if (!cs.ok) {
		return 0;
	}

	pthread_mutex_lock(&s->mu);
	wf_target *t = target < (uint32_t)s->ntargets ? s->targets[target] : NULL;
	int alive = t != NULL && t->alive;
	pthread_mutex_unlock(&s->mu);
	if (t == NULL) {
		return 0;
	}

	pthread_mutex_lock(&s->sym_mu);
	ensure_locked(t);
	if (wf_cs_is_null(t->symbolicator)) {
		pthread_mutex_unlock(&s->sym_mu);
		return 0;
	}

	wf_cs_ref owner = cs.owner_at(t->symbolicator, addr, WF_CS_NOW);
	if (cs.is_null(owner) && alive) {
		// The address is in no image we know. The program may have loaded a
		// library since the symbolicator was built; rebuild it, at most
		// four times a second.
		uint64_t now = mach_absolute_time();
		uint64_t quarter_second = 250000000ULL * s->tb.denom / s->tb.numer;
		if (now - t->sym_refreshed > quarter_second) {
			t->sym_refreshed = now;
			wf_cs_ref fresh = wf_cs_create(t->task);
			if (!wf_cs_is_null(fresh)) {
				cs.release(t->symbolicator);
				t->symbolicator = fresh;
				owner = cs.owner_at(t->symbolicator, addr, WF_CS_NOW);
			}
		}
	}
	if (cs.is_null(owner) && in_shared_cache(addr)) {
		// Code in the shared cache that belongs to no library: the stub
		// islands through which system libraries call each other.
		out->stub = 1;
		strlcpy(out->module, "dyld shared cache", sizeof(out->module));
		strlcpy(out->module_path, "/System/Library/dyld/", sizeof(out->module_path));
		pthread_mutex_unlock(&s->sym_mu);
		return 0;
	}
	if (!cs.is_null(owner)) {
		const char *m = cs.owner_name(owner);
		if (m != NULL) {
			strlcpy(out->module, m, sizeof(out->module));
		}
		const char *mp = cs.owner_path ? cs.owner_path(owner) : NULL;
		if (mp != NULL) {
			strlcpy(out->module_path, mp, sizeof(out->module_path));
		}
	}

	wf_cs_ref sym = cs.symbol_at(t->symbolicator, addr, WF_CS_NOW);
	if (!cs.is_null(sym)) {
		const char *n = cs.symbol_name(sym);
		if (n != NULL && n[0] != '\0') {
			strlcpy(out->name, n, sizeof(out->name));
			wf_cs_range r = cs.symbol_range(sym);
			out->start = r.location;
			out->len = r.length;
			out->found = 1;
		}
	}

	if (out->found && cs.source_at && cs.source_path && cs.source_line) {
		wf_cs_ref src = cs.source_at(t->symbolicator, addr, WF_CS_NOW);
		if (!cs.is_null(src)) {
			const char *p = cs.source_path(src);
			if (p != NULL) {
				strlcpy(out->file, p, sizeof(out->file));
			}
			out->line = cs.source_line(src);
		}
	}
	pthread_mutex_unlock(&s->sym_mu);
	return (int)out->found;
}
