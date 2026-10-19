/* libFuzzer harness: arbitrary agent-area contents must never crash the
 * process-side readers (poll slow path, grant_get, lease, wait-word load).
 *   clang -std=gnu11 -g -O1 -fsanitize=fuzzer,address,undefined c/tests/fuzz_area.c -o fuzz_area
 *   ./fuzz_area -max_total_time=60 */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <string.h>

#define AREA 8192
static _Alignas(64) uint8_t g_area[AREA];
static _Alignas(8) uint8_t g_state[PROCCTL_STATE_STORAGE(8, 128)];

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size) {
  memset(g_area, 0, sizeof g_area);
  memcpy(g_area, data, size < AREA ? size : AREA);
  procctl_t ctx;
  memset(&ctx, 0, sizeof ctx);
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)ctx.opaque;
  c->area = g_area;
  c->area_size = AREA;
  c->map = g_area;
  c->seq = (const uint64_t *)(const void *)g_area;
  c->slots = (uint16_t)(size ? data[0] % 9 : 0);
  c->state = g_state;
  c->state_block_size = (uint32_t)PROCCTL_STATE_BLOCK_SIZE(8, 128);
  c->grant_track = g_state + c->state_block_size;
  c->ready = 1;

  uint8_t buf[512];
  uint64_t gen;
  procctl_poll(&ctx, buf, sizeof buf, &gen);
  procctl_poll(&ctx, buf, 3, &gen);
  uint8_t id[16];
  memset(id, size ? data[size - 1] : 0, sizeof id);
  procctl_grant_t g;
  if (procctl_grant_get(&ctx, 1, id, &g) == 0) procctl_grant_usable(&ctx, &g, 1);
  procctl_lease_expired(&ctx, 12345);
  procctl_agent_attached(&ctx);
  return 0;
}
