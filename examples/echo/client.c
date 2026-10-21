/* echo client: a minimal procctl consumer.
 *
 * Polls for configuration from the agent, prints it, acknowledges it, and
 * publishes "applied gen=N bytes=M" as its consumer state. Falls back to its
 * defaults when the agent's lease expires.
 *
 *   cc -O2 -o echo-client client.c -I../../c
 *   ./echo-client [--interval-ms 100] [--fork-after-ms N [--reinit]] [--exit-after-ms N]
 */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "procctl.h"

#include <errno.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define STATE_CAP 256
static _Alignas(8) unsigned char g_state[PROCCTL_STATE_SIZE(STATE_CAP)];
static procctl_t g_ctx;

static void say(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
static void say(const char *fmt, ...) {
  va_list ap;
  va_start(ap, fmt);
  printf("[echo-client pid=%d] ", getpid());
  vprintf(fmt, ap);
  printf("\n");
  fflush(stdout);
  va_end(ap);
}

static void run(long interval_ms, long exit_after_ms, long fork_after_ms, int reinit) {
  char buf[4096];
  int was_expired = -1;
  uint64_t start = procctl_now_ns();
  int forked = 0;
  for (;;) {
    uint64_t now = procctl_now_ns();
    long elapsed_ms = (long)((now - start) / 1000000);
    if (exit_after_ms > 0 && elapsed_ms >= exit_after_ms) return;

    if (!forked && fork_after_ms > 0 && elapsed_ms >= fork_after_ms) {
      forked = 1;
      pid_t pid = fork();
      if (pid == 0) {
        int r = procctl_ack(&g_ctx, 1);
        say("forked child: state writes refused (%s)", r == -ECHILD ? "-ECHILD" : "unexpected");
        if (reinit) {
          r = procctl_reinit(&g_ctx);
          say("forked child: reinit -> %d (new region)", r);
        }
        fork_after_ms = 0;
        was_expired = -1;
        continue;
      }
      say("forked child pid=%d", pid);
    }

    bool expired = procctl_lease_expired(&g_ctx, now);
    if ((int)expired != was_expired) {
      say(expired ? "lease expired or no agent: using defaults" : "agent lease active");
      was_expired = expired;
    }

    uint64_t gen = 0;
    int n = procctl_poll(&g_ctx, buf, sizeof buf - 1, &gen);
    if (n > 0) {
      buf[n] = 0;
      say("config gen=%llu: %s", (unsigned long long)gen, buf);
      char st[STATE_CAP];
      int len = snprintf(st, sizeof st, "applied gen=%llu bytes=%d", (unsigned long long)gen, n);
      procctl_state_set(&g_ctx, st, (size_t)len);
      procctl_ack(&g_ctx, gen);
    } else if (n == -ENODATA) {
      say("config gen=%llu: none (agent cleared config): using defaults", (unsigned long long)gen);
      procctl_ack(&g_ctx, gen);
    } else if (n < 0 && n != -EAGAIN) {
      say("poll error %d", n);
    }
    usleep((useconds_t)interval_ms * 1000);
  }
}

int main(int argc, char **argv) {
  long interval_ms = 100, exit_after_ms = 0, fork_after_ms = 0;
  int reinit = 0;
  for (int i = 1; i < argc; i++) {
    if (!strcmp(argv[i], "--interval-ms") && i + 1 < argc) interval_ms = atol(argv[++i]);
    else if (!strcmp(argv[i], "--exit-after-ms") && i + 1 < argc) exit_after_ms = atol(argv[++i]);
    else if (!strcmp(argv[i], "--fork-after-ms") && i + 1 < argc) fork_after_ms = atol(argv[++i]);
    else if (!strcmp(argv[i], "--reinit")) reinit = 1;
    else {
      fprintf(stderr, "usage: %s [--interval-ms N] [--exit-after-ms N] [--fork-after-ms N [--reinit]]\n", argv[0]);
      return 2;
    }
  }
  procctl_config_t cfg = {.consumer = "echo", .schema_version = 1, .payload_capacity = 4096,
                          .state_capacity = STATE_CAP,
                          .state_storage = g_state};
  int r = procctl_init(&g_ctx, &cfg);
  if (r) {
    fprintf(stderr, "procctl_init: %s\n", strerror(-r));
    return 1;
  }
  say("ready (region procctl:echo-v1)");
  run(interval_ms, exit_after_ms, fork_after_ms, reinit);
  while (waitpid(-1, NULL, WNOHANG) > 0) {
  }
  return 0;
}
