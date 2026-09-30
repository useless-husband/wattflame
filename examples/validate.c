// validate: does the profile agree with the kernel?
//
// Four phases run one after another. After each phase the program asks the
// kernel how much energy the whole process has used so far, which gives the
// true cost of that phase without any sampling. `make validate` records the
// program with wattflame and compares the energy wattflame attributed to each
// phase function against those numbers.

#include <libproc.h>
#include <pthread.h>
#include <pthread/qos.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/resource.h>
#include <time.h>
#include <unistd.h>

static double now(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

static uint64_t process_energy_nj(void) {
	struct rusage_info_v6 ri;
	if (proc_pid_rusage(getpid(), RUSAGE_INFO_V6, (rusage_info_t *)&ri) != 0) {
		return 0;
	}
	return ri.ri_energy_nj;
}

__attribute__((noinline)) static double float_loop(double seconds) {
	double s = 0, end = now() + seconds;
	unsigned long i = 1;
	while (now() < end) {
		for (int k = 0; k < 100000; k++, i++) {
			s += 1.0 / (double)i + (double)i * 1e-9;
		}
	}
	return s;
}

__attribute__((noinline)) static uint64_t integer_loop(double seconds) {
	uint64_t x = 0x9e3779b97f4a7c15ULL;
	double end = now() + seconds;
	while (now() < end) {
		for (int k = 0; k < 100000; k++) {
			x ^= x >> 33;
			x *= 0xff51afd7ed558ccdULL;
			x += (uint64_t)k;
		}
	}
	return x;
}

// Floating-point work on a performance core.
__attribute__((noinline)) static void phase_float(double seconds) {
	volatile double r = float_loop(seconds);
	(void)r;
}

// Integer work on a performance core.
__attribute__((noinline)) static void phase_integer(double seconds) {
	volatile uint64_t r = integer_loop(seconds);
	(void)r;
}

// The same integer work, demoted to the efficiency cores.
__attribute__((noinline)) static void phase_background(double seconds) {
	pthread_set_qos_class_self_np(QOS_CLASS_BACKGROUND, 0);
	volatile uint64_t r = integer_loop(seconds);
	(void)r;
	pthread_set_qos_class_self_np(QOS_CLASS_USER_INITIATED, 0);
}

// Short bursts separated by sleeps: mostly idle.
__attribute__((noinline)) static void phase_bursty(double seconds) {
	double end = now() + seconds;
	while (now() < end) {
		volatile uint64_t r = integer_loop(0.001);
		(void)r;
		usleep(9000);
	}
}

typedef void (*phase_fn)(double);

int main(int argc, char **argv) {
	double seconds = argc > 1 ? atof(argv[1]) : 1.5;
	const char *names[] = {"phase_float", "phase_integer", "phase_background", "phase_bursty"};
	phase_fn phases[] = {phase_float, phase_integer, phase_background, phase_bursty};

	pthread_set_qos_class_self_np(QOS_CLASS_USER_INITIATED, 0);
	for (int i = 0; i < 4; i++) {
		// A sleep before each reading puts the thread off its core, which
		// is when the kernel brings the process total up to date.
		usleep(20000);
		uint64_t before = process_energy_nj();
		phases[i](seconds);
		usleep(20000);
		uint64_t after = process_energy_nj();
		printf("truth %s %llu\n", names[i], (unsigned long long)((after - before) / 1000));
	}
	return 0;
}
