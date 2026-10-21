/* procctl-testclient: the in-process side of the e2e tests.
 *
 * Polls for payloads from the test agent, checks each one against its
 * self-checking pattern, acks it, and publishes a status block. Logs JSON
 * lines to stdout (CLOCK_BOOTTIME timestamps), which the e2e suite reads.
 *
 *   procctl-testclient [--consumer NAME] [--poll-us 5000] [--exit-after-s N]
 *                      [--fork-after-ms N [--fork-workers K] [--fork-reinit]]
 *
 * SPDX-License-Identifier: Apache-2.0 */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "procctl.h"
#include "testclient.h"

#include <errno.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define PAYLOAD_CAP 65536

static _Alignas(8) unsigned char g_storage[PROCCTL_STATE_SIZE(sizeof(struct testclient_status))];
static procctl_t g_ctx;
static struct testclient_status g_st;

static void logj(const char *ev, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
static void logj(const char *ev, const char *fmt, ...) {
  printf("{\"ev\":\"%s\",\"pod\":\"%s\",\"pid\":%d,\"ts\":%llu", ev, (const char *)g_st.pod, getpid(),
         (unsigned long long)procctl_now_ns());
  if (fmt && *fmt) {
    va_list ap;
    va_start(ap, fmt);
    putchar(',');
    vprintf(fmt, ap);
    va_end(ap);
  }
  printf("}\n");
  fflush(stdout);
}

static uint64_t self_start_time(void) {
  char buf[1024];
  FILE *f = fopen("/proc/self/stat", "r");
  if (!f) return 0;
  size_t n = fread(buf, 1, sizeof buf - 1, f);
  fclose(f);
  buf[n] = 0;
  char *p = strrchr(buf, ')');
  if (!p) return 0;
  int field = 2; /* after ") " comes field 3; we want field 22 */
  for (; *p && field < 22; p++)
    if (*p == ' ') field++;
  return strtoull(p, NULL, 10);
}

static int verify(const uint8_t *b, int n, uint64_t *tag, uint64_t *write_ns) {
  struct testclient_payload h;
  if ((size_t)n < sizeof h) return 0;
  memcpy(&h, b, sizeof h);
  if (sizeof h + h.pattern_len != (size_t)n) return 0;
  for (uint32_t i = 0; i < h.pattern_len; i++)
    if (b[sizeof h + i] != testclient_pattern(h.seq_tag, i)) return 0;
  *tag = h.seq_tag;
  *write_ns = h.agent_write_ns;
  return 1;
}

/* Forks workers that keep running as clients. With reinit, each gets its own
 * region (and reports as "<pod>-childK"); without, it keeps the inherited one,
 * whose state block it may not write. Returns 1 in a child. */
static int fork_workers(long workers, int reinit) {
  for (long k = 0; k < workers; k++) {
    pid_t c = fork();
    if (c < 0) return 0;
    if (c > 0) {
      logj("forked", "\"child\":%d", c);
      continue;
    }
    int ack = procctl_ack(&g_ctx, 1); /* refused: -ECHILD */
    int rr = -1;
    if (reinit) {
      char parent[64];
      snprintf(parent, sizeof parent, "%s", (const char *)g_st.pod);
      rr = procctl_reinit(&g_ctx);
      snprintf((char *)g_st.pod, sizeof g_st.pod, "%.48s-child%ld", parent, k % 1000);
      g_st.ns_pid = getpid();
      g_st.start_time = self_start_time();
      g_st.last_gen = 0;
      procctl_state_set(&g_ctx, &g_st, sizeof g_st);
    }
    logj("forked_child", "\"reinit\":%d,\"inherited_ack\":%d", rr, ack);
    return 1;
  }
  return 0;
}

int main(int argc, char **argv) {
  long poll_us = 5000, exit_after_s = 0, fork_after_ms = 0, workers = 1;
  int reinit = 0;
  const char *consumer = TESTCLIENT_CONSUMER;
  for (int i = 1; i < argc; i++) {
    /* "--opt value" or "--opt=value" */
    char name[64];
    const char *val = NULL, *eq = strchr(argv[i], '=');
    if (eq && (size_t)(eq - argv[i]) < sizeof name) {
      memcpy(name, argv[i], (size_t)(eq - argv[i]));
      name[eq - argv[i]] = 0;
      val = eq + 1;
    } else {
      snprintf(name, sizeof name, "%s", argv[i]);
    }
#define NEXT() (val ? val : (i + 1 < argc ? argv[++i] : "0"))
    if (!strcmp(name, "--consumer")) consumer = NEXT();
    else if (!strcmp(name, "--poll-us")) poll_us = atol(NEXT());
    else if (!strcmp(name, "--exit-after-s")) exit_after_s = atol(NEXT());
    else if (!strcmp(name, "--fork-after-ms")) fork_after_ms = atol(NEXT());
    else if (!strcmp(name, "--fork-workers")) workers = atol(NEXT());
    else if (!strcmp(name, "--fork-reinit")) reinit = 1;
    else {
      fprintf(stderr, "usage: %s [--consumer NAME] [--poll-us N] [--exit-after-s N]"
                      " [--fork-after-ms N [--fork-workers K] [--fork-reinit]]\n", argv[0]);
      return 2;
    }
#undef NEXT
  }
  const char *pod = getenv("POD_NAME");
  snprintf((char *)g_st.pod, sizeof g_st.pod, "%s", pod ? pod : "local");
  g_st.ns_pid = getpid();
  g_st.start_time = self_start_time();

  procctl_config_t cfg = {.consumer = consumer, .schema_version = TESTCLIENT_SCHEMA_VERSION,
                          .payload_capacity = PAYLOAD_CAP, .state_capacity = sizeof g_st,
                          .state_storage = g_storage};
  int r = procctl_init(&g_ctx, &cfg);
  if (r) {
    logj("init_failed", "\"err\":%d", r);
    return 1;
  }
  uint64_t start = procctl_now_ns();
  logj("start", "\"start_time\":%llu", (unsigned long long)g_st.start_time);
  procctl_state_set(&g_ctx, &g_st, sizeof g_st);

  static uint8_t buf[PAYLOAD_CAP];
  int was_expired = -1, first = 1;
  for (;;) {
    uint64_t now = procctl_now_ns();
    if (exit_after_s && now - start > (uint64_t)exit_after_s * 1000000000ull) break;
    if (fork_after_ms > 0 && now - start > (uint64_t)fork_after_ms * 1000000ull) {
      fork_after_ms = 0;
      if (fork_workers(workers, reinit)) { /* in a child: start over */
        first = 1;
        was_expired = -1;
        start = procctl_now_ns();
      }
    }
    int dirty = 0;
    uint64_t gen = 0;
    int n = procctl_poll(&g_ctx, buf, sizeof buf, &gen);
    if (n > 0) {
      uint64_t tag = 0, wns = 0, recv = procctl_now_ns();
      g_st.changes++;
      if (gen < g_st.last_gen) g_st.backwards++;
      else if (gen == g_st.last_gen) g_st.redelivered++;
      if (!verify(buf, n, &tag, &wns)) {
        g_st.torn++;
        logj("torn", "\"gen\":%llu,\"len\":%d", (unsigned long long)gen, n);
      } else {
        g_st.last_seq_tag = tag;
        g_st.last_agent_write_ns = wns;
        g_st.last_recv_ns = recv;
        if (first) {
          first = 0;
          logj("first_apply", "\"gen\":%llu,\"since_start_ms\":%.1f", (unsigned long long)gen,
               (double)(recv - start) / 1e6);
        }
      }
      g_st.last_gen = gen;
      dirty = 1;
    }
    int expired = procctl_lease_expired(&g_ctx, now);
    if (expired != was_expired) {
      g_st.lease_expired = (uint32_t)expired;
      logj("lease", "\"expired\":%d", expired);
      was_expired = expired;
      dirty = 1;
    }
    if (dirty) procctl_state_set(&g_ctx, &g_st, sizeof g_st);
    if (n > 0) procctl_ack(&g_ctx, gen);
    usleep((useconds_t)poll_us);
  }
  logj("exit", "\"torn\":%llu,\"changes\":%llu", (unsigned long long)g_st.torn,
       (unsigned long long)g_st.changes);
  return 0;
}
