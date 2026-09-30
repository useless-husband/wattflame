// Target program for the sampler tests.
//
//   testprog spin <seconds> [exit code]    two threads, hot_a on the main one and hot_b on a worker
//   testprog touch <file> <seconds>        create <file> first thing in main, then spin
//   testprog fork <seconds>                fork; the child runs hot_child, the parent hot_a
//   testprog nested <seconds>              hot_outer calls the frameless leaf_inner
//   testprog system <seconds>              run `sleep <seconds>` through /bin/sh while spinning

#include <fcntl.h>
#include <pthread.h>
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
	double seconds = atof(argv[2]);
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
