// Target program for the sampler tests.
//
//   testprog spin <seconds> [exit code]    two threads, hot_a on the main one and hot_b on a worker
//   testprog touch <file> <seconds>        create <file> first thing in main, then spin
//   testprog fork <seconds>                fork; the child runs hot_child, the parent hot_a
//   testprog nested <seconds>              hot_outer calls the frameless leaf_inner
//   testprog system <seconds>              run `sleep <seconds>` through /bin/sh while spinning
//   testprog noop x                        exit at once
//   testprog spawnmany <n>                 run `testprog noop x` n times, one after another
//   testprog threads <n>                   n threads in a row, each busy for about 4 ms
//   testprog execchain <seconds>           hot_a, then exec into `testprog spin <seconds>`
//   testprog setuid <seconds>              run the setuid-root /usr/bin/top once while spinning
//   testprog spoof <seconds>               hot_a, then have a helper send a bogus handshake in
//                                          this process's name, then hot_b

#include <fcntl.h>
#include <mach/mach.h>
#include <pthread.h>
#include <servers/bootstrap.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static double now(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

#define SPIN(expr)                                   \
	double s = 0, end = now() + seconds;         \
	unsigned long i = 1;                         \
	while (now() < end) {                        \
		for (int k = 0; k < 50000; k++, i++) \
			s += (expr);                 \
	}                                            \
	return s;

__attribute__((noinline)) double hot_a(double seconds) { SPIN(1.0 / (double)i) }
__attribute__((noinline)) double hot_b(double seconds) { SPIN((double)i * 0.5) }
__attribute__((noinline)) double hot_child(double seconds) { SPIN((double)(i % 7)) }

// Small enough that the compiler gives it no stack frame, so the sampler has
// to find its caller through the link register.
__attribute__((noinline)) unsigned long leaf_inner(unsigned long x) {
	x ^= x >> 13;
	x *= 0x9e3779b97f4a7c15UL;
	return x ^ (x >> 29);
}
__attribute__((noinline)) double hot_outer(double seconds) {
	unsigned long x = 1;
	double end = now() + seconds;
	while (now() < end) {
		for (int k = 0; k < 200000; k++)
			x = leaf_inner(x + (unsigned long)k);
	}
	return (double)x;
}

extern char **environ;

static void *brief(void *arg) {
	(void)arg;
	volatile double r = hot_b(0.004);
	(void)r;
	return NULL;
}

// What a hostile neighbour could do: claim to be `victim` using a task *name*
// port, which any process of the same user can obtain for any other.
static void send_bogus_handshake(pid_t victim) {
	const char *name = getenv("WATTFLAME_PORT");
	mach_port_t server = MACH_PORT_NULL, fake = MACH_PORT_NULL;
	if (name == NULL || bootstrap_look_up(bootstrap_port, name, &server) != KERN_SUCCESS) {
		return;
	}
	if (task_name_for_pid(mach_task_self(), victim, &fake) != KERN_SUCCESS) {
		return;
	}
	struct {
		mach_msg_header_t hdr;
		mach_msg_body_t body;
		mach_msg_port_descriptor_t task;
		int32_t pid;
	} msg;
	memset(&msg, 0, sizeof(msg));
	msg.hdr.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, 0);
	msg.hdr.msgh_size = sizeof(msg);
	msg.hdr.msgh_remote_port = server;
	msg.hdr.msgh_id = 0x57464c4d;
	msg.body.msgh_descriptor_count = 1;
	msg.task.name = fake;
	msg.task.disposition = MACH_MSG_TYPE_COPY_SEND;
	msg.task.type = MACH_MSG_PORT_DESCRIPTOR;
	msg.pid = victim;
	mach_msg(&msg.hdr, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof(msg), 0, MACH_PORT_NULL, 1000,
	         MACH_PORT_NULL);
}

static void *worker(void *arg) {
	pthread_setname_np("test-worker");
	volatile double r = hot_b(*(double *)arg);
	(void)r;
	return NULL;
}

int main(int argc, char **argv) {
	if (argc < 3) {
		return 64;
	}
	if (strcmp(argv[1], "touch") == 0) {
		int fd = open(argv[2], O_CREAT | O_WRONLY, 0644);
		if (fd >= 0) {
			close(fd);
		}
		volatile double r = hot_a(argc > 3 ? atof(argv[3]) : 0.1);
		(void)r;
		return 0;
	}
	if (strcmp(argv[1], "noop") == 0) {
		return 0;
	}
	if (strcmp(argv[1], "spawnmany") == 0) {
		char *child_argv[] = {argv[0], "noop", "x", NULL};
		for (int i = 0; i < atoi(argv[2]); i++) {
			pid_t pid;
			if (posix_spawn(&pid, argv[0], NULL, NULL, child_argv, environ) == 0) {
				int status;
				waitpid(pid, &status, 0);
			}
		}
		return 0;
	}
	if (strcmp(argv[1], "threads") == 0) {
		for (int i = 0; i < atoi(argv[2]); i++) {
			pthread_t t;
			pthread_create(&t, NULL, brief, NULL);
			pthread_join(t, NULL);
		}
		return 0;
	}
	double seconds = atof(argv[2]);
	if (strcmp(argv[1], "execchain") == 0) {
		volatile double r = hot_a(seconds);
		(void)r;
		execl(argv[0], argv[0], "spin", argv[2], (char *)NULL);
		return 127;
	}
	if (strcmp(argv[1], "setuid") == 0) {
		pid_t pid;
		char *top_argv[] = {"top", "-l", "1", "-n", "0", NULL};
		int ok = posix_spawn(&pid, "/usr/bin/top", NULL, NULL, top_argv, environ) == 0;
		volatile double r = hot_a(seconds);
		(void)r;
		if (ok) {
			int status;
			waitpid(pid, &status, 0);
		}
		return 0;
	}
	if (strcmp(argv[1], "spoof") == 0) {
		volatile double r = hot_a(seconds);
		(void)r;
		pid_t pid = fork();
		if (pid == 0) {
			send_bogus_handshake(getppid());
			_exit(0);
		}
		int status;
		waitpid(pid, &status, 0);
		usleep(50000);
		r = hot_b(seconds);
		return 0;
	}
	if (strcmp(argv[1], "fork") == 0) {
		pid_t pid = fork();
		if (pid == 0) {
			volatile double r = hot_child(seconds);
			(void)r;
			_exit(0);
		}
		volatile double r = hot_a(seconds);
		(void)r;
		int status;
		waitpid(pid, &status, 0);
		return 0;
	}
	if (strcmp(argv[1], "system") == 0) {
		char cmd[64];
		snprintf(cmd, sizeof(cmd), "sleep %s", argv[2]);
		pid_t pid = fork();
		if (pid == 0) {
			execl("/bin/sh", "sh", "-c", cmd, (char *)NULL);
			_exit(127);
		}
		volatile double r = hot_a(seconds);
		(void)r;
		int status;
		waitpid(pid, &status, 0);
		return 0;
	}
	if (strcmp(argv[1], "nested") == 0) {
		volatile double r = hot_outer(seconds);
		(void)r;
		return 0;
	}
	pthread_t t;
	pthread_create(&t, NULL, worker, &seconds);
	volatile double r = hot_a(seconds);
	(void)r;
	pthread_join(t, NULL);
	return argc > 3 ? atoi(argv[3]) : 0;
}
