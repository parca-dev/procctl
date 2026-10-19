/* wait_client: a control thread blocks in procctl_wait and mirrors the grant
 * for (kind 1, id 0x42 x16) into its resource table: HOLDING while usable,
 * RELEASED otherwise. Used by the Go interop tests. Exits on stdin EOF. */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static _Alignas(8) unsigned char g_state[PROCCTL_STATE_STORAGE(2, 64)];
static procctl_t g_ctx;

static void *ctl(void *arg) {
  (void)arg;
  uint8_t id[16];
  memset(id, 0x42, sizeof id);
  uint64_t held = 0;
  procctl_resource_report(&g_ctx, 1, id, PROCCTL_RES_IDLE, 0, PROCCTL_RES_F_WANT, 0, 0, 0);
  for (;;) {
    procctl_grant_t g;
    if (procctl_grant_get(&g_ctx, 1, id, &g) == 0) {
      bool ok = g.grant_id && procctl_grant_usable(&g_ctx, &g, 0) && !procctl_lease_expired(&g_ctx, 0);
      if (ok && held != g.grant_id) {
        held = g.grant_id;
        procctl_resource_report(&g_ctx, 1, id, PROCCTL_RES_HOLDING, held, PROCCTL_RES_F_WANT, 0,
                                g.seen_gen, 0);
      } else if (!ok && held) {
        procctl_resource_report(&g_ctx, 1, id, PROCCTL_RES_RELEASED, held, PROCCTL_RES_F_WANT, 0,
                                g.seen_gen, 0);
        held = 0;
      } else {
        procctl_resource_report(&g_ctx, 1, id, held ? PROCCTL_RES_HOLDING : PROCCTL_RES_IDLE, held,
                                PROCCTL_RES_F_WANT, 0, g.seen_gen, 0);
      }
    }
    procctl_wait(&g_ctx, 250000000ull);
  }
  return NULL;
}

int main(void) {
  procctl_config_t cfg = {.consumer = "waittest", .schema_version = 1, .payload_capacity = 128,
                          .resource_slots = 2, .state_capacity = 64, .state_storage = g_state};
  if (procctl_init(&g_ctx, &cfg)) return 1;
  pthread_t th;
  pthread_create(&th, NULL, ctl, NULL);
  printf("ready\n");
  fflush(stdout);
  char b[16];
  while (read(0, b, sizeof b) > 0) {
  }
  return 0;
}
