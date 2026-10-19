/* token client: an exclusive-resource holder (SPEC §9.1).
 *
 * A control thread runs the reference holder loop: it sleeps in procctl_wait,
 * and acquires or releases a simulated resource as grants come and go. The
 * application's own thread is never involved (H6). Every acquire and release
 * is logged to stdout as JSON with CLOCK_BOOTTIME timestamps, so tests can
 * check for overlaps without trusting the agent.
 *
 *   cc -O2 -o token-client client.c -I../../c -lpthread
 *   ./token-client [--acquire-ms 300] [--no-transfer] [--no-want]
 */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "procctl.h"

#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define KIND 1
static _Alignas(8) unsigned char g_state[PROCCTL_STATE_STORAGE(1, 64)];
static procctl_t g_ctx;
static uint8_t g_id[16];
static long g_acquire_ms = 300;
static int g_transfer = 1;
static int g_want = 1;

static void logev(const char *ev, uint64_t grant, uint64_t ts) {
  printf("{\"ev\":\"%s\",\"pid\":%d,\"grant\":%llu,\"ts\":%llu}\n", ev, getpid(),
         (unsigned long long)grant, (unsigned long long)ts);
  fflush(stdout);
}

static void report(uint32_t state, uint64_t grant, uint64_t seen) {
  procctl_resource_report(&g_ctx, KIND, g_id, state, grant, g_want ? PROCCTL_RES_F_WANT : 0, 0,
                          seen, 0);
}

static void *control_thread(void *arg) {
  (void)arg;
  sigset_t all;
  sigfillset(&all);
  pthread_sigmask(SIG_BLOCK, &all, NULL);
  pthread_setname_np(pthread_self(), "token-ctl");

  uint64_t held = 0, seen = 0;
  report(PROCCTL_RES_IDLE, 0, 0);
  for (;;) {
    procctl_grant_t g;
    if (procctl_grant_get(&g_ctx, KIND, g_id, &g) != 0) {
      procctl_wait(&g_ctx, 10000000ull);
      continue;
    }
    seen = g.seen_gen;
    bool ok = g.grant_id && procctl_grant_usable(&g_ctx, &g, 0) && !procctl_lease_expired(&g_ctx, 0);
    if (held && (!ok || g.grant_id != held)) {
      if (ok && g_transfer) { /* renewal: keep using the resource */
        logev("transfer", g.grant_id, procctl_now_ns());
        held = g.grant_id;
        report(PROCCTL_RES_HOLDING, held, seen);
      } else {
        logev("release", held, procctl_now_ns()); /* stop using it, then report */
        report(PROCCTL_RES_RELEASED, held, seen);
        held = 0;
      }
    } else if (!held && ok && g_want) {
      report(PROCCTL_RES_ACQUIRING, g.grant_id, seen); /* H2: announce first */
      logev("acquire", g.grant_id, procctl_now_ns());
      usleep((useconds_t)g_acquire_ms * 1000); /* e.g. CUPTI Enable + Start */
      held = g.grant_id;
      report(PROCCTL_RES_HOLDING, held, seen);
    } else {
      report(held ? PROCCTL_RES_HOLDING : PROCCTL_RES_IDLE, held, seen); /* H4 */
    }
    procctl_wait(&g_ctx, 50000000ull); /* well under lease/4 and any grant */
  }
  return NULL;
}

int main(int argc, char **argv) {
  memset(g_id, 0x42, sizeof g_id);
  for (int i = 1; i < argc; i++) {
    if (!strcmp(argv[i], "--acquire-ms") && i + 1 < argc) g_acquire_ms = atol(argv[++i]);
    else if (!strcmp(argv[i], "--no-transfer")) g_transfer = 0;
    else if (!strcmp(argv[i], "--no-want")) g_want = 0;
    else {
      fprintf(stderr, "usage: %s [--acquire-ms N] [--no-transfer] [--no-want]\n", argv[0]);
      return 2;
    }
  }
  procctl_config_t cfg = {.consumer = "token", .schema_version = 1, .payload_capacity = 64,
                          .resource_slots = 1, .state_capacity = 64, .state_storage = g_state};
  if (procctl_init(&g_ctx, &cfg)) return 1;
  pthread_t th;
  pthread_create(&th, NULL, control_thread, NULL);
  printf("{\"ev\":\"ready\",\"pid\":%d}\n", getpid());
  fflush(stdout);
  for (;;) pause(); /* the application: busy with its own work, never calls us */
}
