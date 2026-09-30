// libwattflame_preload.dylib
//
// Injected into the profiled program with DYLD_INSERT_LIBRARIES. Its only job
// is to hand the program's Mach task port to the wattflame process, which is
// the one thing an unprivileged profiler cannot obtain on its own (task_for_pid
// needs root). After the handshake it does nothing: all sampling happens from
// the outside.
//
// The handshake is synchronous. The constructor blocks until wattflame replies,
// so sampling is already running when main() starts.
//
// The environment variable that loads this library is inherited by every
// child process, including ones this library must not touch. dyld refuses to
// start a process when an inserted library lacks its architecture, so the
// library is built for arm64, arm64e and x86_64 even though only arm64
// processes are sampled. And a process whose task port the kernel has made
// immovable is killed outright for trying to send it, so those stay silent.
// wattflame still counts the energy of the processes that do not check in.

#if defined(__arm64__)

#include <mach/mach.h>
#include <pthread.h>
#include <servers/bootstrap.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>

#define WF_MSG_HELLO 0x57464c4d // "WFLM"
#define WF_ENV_PORT "WATTFLAME_PORT"

// Code-signing status of a process (xnu bsd/sys/codesign.h).
extern int csops(pid_t pid, unsigned int ops, void *useraddr, size_t usersize);
#define WF_CS_OPS_STATUS 0
#define WF_CS_RUNTIME 0x00010000u
#define WF_CS_PLATFORM_BINARY 0x04000000u

typedef struct {
	mach_msg_header_t hdr;
	mach_msg_body_t body;
	mach_msg_port_descriptor_t task;
	int32_t pid;
} wf_hello_t;

typedef struct {
	mach_msg_header_t hdr;
	mach_msg_trailer_t trailer;
} wf_reply_t;

// The task port of Apple's own binaries (the compiler and linker among them)
// and of hardened-runtime apps is immovable: sending it raises a fatal
// EXC_GUARD. When in doubt, do not send.
static int task_port_is_movable(void) {
	uint32_t flags = 0;
	if (csops(getpid(), WF_CS_OPS_STATUS, &flags, sizeof(flags)) != 0) {
		return 0;
	}
	return (flags & (WF_CS_PLATFORM_BINARY | WF_CS_RUNTIME)) == 0;
}

static void wf_hello(void) {
	const char *name = getenv(WF_ENV_PORT);
	if (name == NULL || name[0] == '\0') {
		return;
	}
	if (!task_port_is_movable()) {
		return;
	}

	mach_port_t server = MACH_PORT_NULL;
	if (bootstrap_look_up(bootstrap_port, name, &server) != KERN_SUCCESS) {
		// wattflame is gone (for example a daemon that outlived the
		// recording). Run unprofiled.
		return;
	}

	mach_port_t reply = MACH_PORT_NULL;
	if (mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &reply) != KERN_SUCCESS) {
		mach_port_deallocate(mach_task_self(), server);
		return;
	}

	wf_hello_t msg = {0};
	msg.hdr.msgh_bits = MACH_MSGH_BITS_COMPLEX |
	                    MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
	msg.hdr.msgh_size = sizeof(msg);
	msg.hdr.msgh_remote_port = server;
	msg.hdr.msgh_local_port = reply;
	msg.hdr.msgh_id = WF_MSG_HELLO;
	msg.body.msgh_descriptor_count = 1;
	msg.task.name = mach_task_self();
	msg.task.disposition = MACH_MSG_TYPE_COPY_SEND;
	msg.task.type = MACH_MSG_PORT_DESCRIPTOR;
	msg.pid = getpid();

	kern_return_t kr = mach_msg(&msg.hdr, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof(msg), 0,
	                            MACH_PORT_NULL, 2000, MACH_PORT_NULL);
	if (kr == KERN_SUCCESS) {
		// Wait for the go-ahead. The timeout keeps a crashed profiler from
		// hanging the program forever.
		wf_reply_t r;
		mach_msg(&r.hdr, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof(r), reply, 5000,
		         MACH_PORT_NULL);
	}

	mach_port_mod_refs(mach_task_self(), reply, MACH_PORT_RIGHT_RECEIVE, -1);
	mach_port_deallocate(mach_task_self(), server);
}

__attribute__((constructor)) static void wf_init(void) {
	wf_hello();
	// A fork()ed child is a new task with a new task port; introduce it too.
	pthread_atfork(NULL, NULL, wf_hello);
}

#else

// Intel processes running under Rosetta are not sampled. This slice exists
// only so that dyld can load the library into them and carry on.
__attribute__((unused)) static const int wf_unused;

#endif
