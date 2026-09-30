// pipeline: a small data-processing job with several distinct stages, so the
// flame graph has something to show: string parsing, sorting through the C
// library, hashing, a floating-point kernel, and bulk memory copies.
//
//   wattflame record -- examples/bin/pipeline 3

#include <math.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define RECORDS 60000
#define LINE 48

typedef struct {
	uint32_t id;
	uint32_t score;
	char name[24];
} record;

static double now(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return (double)t.tv_sec + (double)t.tv_nsec / 1e9;
}

static uint64_t rng_state = 88172645463325252ULL;
static uint64_t rng(void) {
	rng_state ^= rng_state << 13;
	rng_state ^= rng_state >> 7;
	rng_state ^= rng_state << 17;
	return rng_state;
}

__attribute__((noinline)) static void make_lines(char *text) {
	for (int i = 0; i < RECORDS; i++) {
		snprintf(text + (size_t)i * LINE, LINE, "%u,%u,user%llu", (unsigned)i, (unsigned)(rng() % 100000),
		         (unsigned long long)(rng() % 1000000));
	}
}

__attribute__((noinline)) static uint32_t parse_number(const char **p) {
	uint32_t v = 0;
	while (**p >= '0' && **p <= '9') {
		v = v * 10 + (uint32_t)(**p - '0');
		(*p)++;
	}
	return v;
}

__attribute__((noinline)) static void parse_records(const char *text, record *out) {
	for (int i = 0; i < RECORDS; i++) {
		const char *p = text + (size_t)i * LINE;
		out[i].id = parse_number(&p);
		p++;
		out[i].score = parse_number(&p);
		p++;
		strlcpy(out[i].name, p, sizeof(out[i].name));
	}
}

static int by_score(const void *a, const void *b) {
	const record *x = a, *y = b;
	if (x->score != y->score) {
		return x->score < y->score ? -1 : 1;
	}
	return strcmp(x->name, y->name);
}

__attribute__((noinline)) static void sort_records(record *r) {
	qsort(r, RECORDS, sizeof(record), by_score);
}

__attribute__((noinline)) static uint64_t hash_bytes(const void *data, size_t n) {
	const uint8_t *p = data;
	uint64_t h = 1469598103934665603ULL;
	for (size_t i = 0; i < n; i++) {
		h = (h ^ p[i]) * 1099511628211ULL;
	}
	return h;
}

__attribute__((noinline)) static uint64_t hash_records(const record *r) {
	uint64_t h = 0;
	for (int i = 0; i < RECORDS; i++) {
		h ^= hash_bytes(&r[i], sizeof(record));
	}
	return h;
}

__attribute__((noinline)) static double smooth_scores(const record *r, double *buf) {
	double acc = 0;
	for (int pass = 0; pass < 6; pass++) {
		for (int i = 1; i < RECORDS - 1; i++) {
			double v = (r[i - 1].score + 2.0 * r[i].score + r[i + 1].score) / 4.0;
			buf[i] = sqrt(v) + log1p(v);
			acc += buf[i];
		}
	}
	return acc;
}

__attribute__((noinline)) static void snapshot(const record *r, record *copy) {
	for (int pass = 0; pass < 12; pass++) {
		memcpy(copy, r, sizeof(record) * RECORDS);
	}
}

typedef struct {
	double seconds;
	uint64_t result;
} job;

__attribute__((noinline)) static void *ingest(void *arg) {
	job *j = arg;
	pthread_setname_np("ingest");
	char *text = malloc((size_t)RECORDS * LINE);
	record *recs = malloc(sizeof(record) * RECORDS);
	for (double end = now() + j->seconds; now() < end;) {
		make_lines(text);
		parse_records(text, recs);
		sort_records(recs);
		j->result ^= hash_records(recs);
	}
	free(text);
	free(recs);
	return NULL;
}

__attribute__((noinline)) static void *analyse(void *arg) {
	job *j = arg;
	pthread_setname_np("analyse");
	record *recs = calloc(RECORDS, sizeof(record));
	record *copy = malloc(sizeof(record) * RECORDS);
	double *buf = malloc(sizeof(double) * RECORDS);
	for (int i = 0; i < RECORDS; i++) {
		recs[i].score = (uint32_t)(rng() % 100000);
	}
	double acc = 0;
	for (double end = now() + j->seconds; now() < end;) {
		acc += smooth_scores(recs, buf);
		snapshot(recs, copy);
	}
	j->result = (uint64_t)acc;
	free(recs);
	free(copy);
	free(buf);
	return NULL;
}

int main(int argc, char **argv) {
	double seconds = argc > 1 ? atof(argv[1]) : 3;
	job a = {seconds, 0}, b = {seconds, 0};
	pthread_t ta, tb;
	pthread_create(&ta, NULL, ingest, &a);
	pthread_create(&tb, NULL, analyse, &b);
	pthread_join(ta, NULL);
	pthread_join(tb, NULL);
	printf("ingest %016llx, analyse %llu\n", (unsigned long long)a.result, (unsigned long long)b.result);
	return 0;
}
