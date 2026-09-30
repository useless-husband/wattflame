// cores: the same function, for the same wall-clock time, on two kinds of core.
//
// One thread runs at the highest quality-of-service class, which macOS
// schedules on performance cores. The other runs at the background class,
// which is confined to efficiency cores. A time profiler shows the two halves
// as equal. An energy profile does not.
//
//   wattflame record -- examples/bin/cores 4

#include <pthread.h>
#include <pthread/qos.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

static double now(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

// A few hundred thousand rounds of integer mixing. Returns how many rounds ran
// so the two threads can report how much work each finished.
__attribute__((noinline)) static uint64_t crunch(double seconds, uint64_t *checksum) {
	uint64_t x = 0x9e3779b97f4a7c15ULL, rounds = 0;
	double end = now() + seconds;
	while (now() < end) {
		for (int i = 0; i < 200000; i++) {
			x ^= x >> 33;
			x *= 0xff51afd7ed558ccdULL;
			x ^= x >> 29;
			x += (uint64_t)i;
		}
		rounds++;
	}
	*checksum = x;
	return rounds;
}

typedef struct {
	double seconds;
	uint64_t rounds;
	uint64_t checksum;
} job;

__attribute__((noinline)) static void *on_performance_cores(void *arg) {
	job *j = arg;
	pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0);
	pthread_setname_np("performance");
	j->rounds = crunch(j->seconds, &j->checksum);
	return NULL;
}

__attribute__((noinline)) static void *on_efficiency_cores(void *arg) {
	job *j = arg;
	pthread_set_qos_class_self_np(QOS_CLASS_BACKGROUND, 0);
	pthread_setname_np("efficiency");
	j->rounds = crunch(j->seconds, &j->checksum);
	return NULL;
}

int main(int argc, char **argv) {
	double seconds = argc > 1 ? atof(argv[1]) : 4;
	job fast = {seconds, 0, 0}, slow = {seconds, 0, 0};
	pthread_t a, b;
	pthread_create(&a, NULL, on_performance_cores, &fast);
	pthread_create(&b, NULL, on_efficiency_cores, &slow);
	pthread_join(a, NULL);
	pthread_join(b, NULL);
	printf("performance cores: %llu rounds\n", (unsigned long long)fast.rounds);
	printf("efficiency cores:  %llu rounds\n", (unsigned long long)slow.rounds);
	return 0;
}
