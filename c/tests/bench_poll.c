/* poll() microbenchmarks.
 *   1. fast path: no change — target < 2 ns
 *   2. slow path: a new generation of 64 B and 4 KiB
 *   3. proxy for a ~5 µs CUDA launch: a 5 µs busy loop with and without a
 *      poll() call in front of it — target: delta within noise */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

static _Alignas(8) uint8_t g_state[PROCCTL_STATE_SIZE(64)];
static procctl_t g_ctx;
static uint8_t *g_area; /* writable view, playing the agent */

static void agent_write(const void *p, uint32_t n) {
  struct procctl_ctrl *cb = (struct procctl_ctrl *)(void *)g_area;
  uint64_t s = cb->seq;
  __atomic_store_n(&cb->seq, s + 1, __ATOMIC_RELAXED);
  __atomic_thread_fence(__ATOMIC_RELEASE);
  memcpy(g_area + sizeof *cb, p, n);
  cb->payload_crc = pc__crc32c(g_area + sizeof *cb, n);
  cb->payload_len = n;
  __atomic_store_n(&cb->seq, s + 2, __ATOMIC_RELEASE);
}

static inline uint64_t now(void) { return procctl_now_ns(); }

static void spin_ns(uint64_t ns) {
  uint64_t end = now() + ns;
  while (now() < end) {
  }
}

int main(void) {
  procctl_config_t cfg = {.consumer = "bench", .payload_capacity = 8192, .state_capacity = 64,
                          .state_storage = g_state};
  if (procctl_init(&g_ctx, &cfg)) return 1;
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)g_ctx.opaque;
  char path[64];
  snprintf(path, sizeof path, "/proc/self/fd/%d", c->fd);
  int fd = open(path, O_RDWR);
  uint8_t *m = mmap(NULL, c->map_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  g_area = m + ((const struct procctl_header *)(const void *)c->map)->page_size;

  static uint8_t buf[8192];
  uint64_t gen;
  volatile int sink = 0;

  /* 1. fast path */
  agent_write("x", 1);
  procctl_poll(&g_ctx, buf, sizeof buf, &gen);
  const long N = 200000000;
  uint64_t t0 = now();
  for (long i = 0; i < N; i++) sink += procctl_poll(&g_ctx, buf, sizeof buf, &gen);
  double fast = (double)(now() - t0) / N;
  printf("poll no-change:        %6.2f ns/op\n", fast);

  /* 2. slow path */
  static uint8_t payload[4096];
  memset(payload, 7, sizeof payload);
  const uint32_t sizes[] = {64, 4096};
  for (int k = 0; k < 2; k++) {
    const long M = 200000;
    uint64_t total = 0;
    for (long i = 0; i < M; i++) {
      agent_write(payload, sizes[k]);
      uint64_t a = now();
      sink += procctl_poll(&g_ctx, buf, sizeof buf, &gen);
      total += now() - a;
    }
    printf("poll new %4u B:       %6.0f ns/op\n", sizes[k], (double)total / M);
  }

  /* 3. 5 µs launch proxy */
  const long L = 200000;
  uint64_t base = 0, with = 0;
  for (int round = 0; round < 4; round++) {
    uint64_t a = now();
    for (long i = 0; i < L / 4; i++) spin_ns(5000);
    base += now() - a;
    a = now();
    for (long i = 0; i < L / 4; i++) {
      sink += procctl_poll(&g_ctx, buf, sizeof buf, &gen);
      spin_ns(5000);
    }
    with += now() - a;
  }
  printf("5 us launch proxy:     %8.1f ns without poll, %8.1f ns with (delta %+.2f ns)\n",
         (double)base / L, (double)with / L, (double)((int64_t)with - (int64_t)base) / L);
  (void)sink;
  /* Informational by default (shared CI runners are noisy). */
  if (getenv("PROCCTL_BENCH_ENFORCE") && fast >= 2.0) {
    fprintf(stderr, "FAIL: no-change poll %.2f ns >= 2 ns\n", fast);
    return 3;
  }
  return 0;
}
