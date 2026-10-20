/* procctl-testclient: the in-process side of the e2e tests.
 *
 * Polls for payloads from the test agent and checks each one against its
 * self-checking pattern, acks it, and publishes a status block. With
 * --exclusive it also competes for a node-wide simulated exclusive resource
 * from a control thread (SPEC §9.1).
 *
 * Everything that matters is logged to stdout as JSON lines with
 * CLOCK_BOOTTIME timestamps; the e2e suite reads them from the pod logs and
 * never relies on the agent's view for correctness checks.
 *
 *   procctl-testclient [--poll-us 1000] [--exclusive [--acquire-ms 300] [--holder-hang]]
 *                      [--exit-after-s N] [--init-delay-ms N]
 *                      [--fork-after-ms N [--fork-workers K] [--fork-reinit]]
 *   procctl-testclient --hostile=MODE     a deliberately broken region (S8)
 *   procctl-testclient --noise-procs=N [--noise-fds=M] [--exec-rate=R]
 *                                         scan and exec load, no region (S9)
 *
 * SPDX-License-Identifier: Apache-2.0 */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "procctl.h"
#include "testclient.h"

#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/wait.h>
#include <unistd.h>

#define SLOTS 1
#define PAYLOAD_CAP 65536

static _Alignas(8) unsigned char g_storage[PROCCTL_STATE_STORAGE(SLOTS, sizeof(struct testclient_status))];
static procctl_t g_ctx;
static struct testclient_status g_st;
static pthread_mutex_t g_st_mu = PTHREAD_MUTEX_INITIALIZER;
static uint8_t g_token_id[16];
static long g_acquire_ms = 300;
static int g_holder_hang;
static pthread_mutex_t g_out_mu = PTHREAD_MUTEX_INITIALIZER;

static void logj(const char *ev, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
static void logj(const char *ev, const char *fmt, ...) {
  pthread_mutex_lock(&g_out_mu);
  printf("{\"ev\":\"%s\",\"pod\":\"%s\",\"pid\":%d,\"ts\":%llu", ev, (const char *)g_st.pod,
         getpid(), (unsigned long long)procctl_now_ns());
  if (fmt && *fmt) {
    va_list ap;
    va_start(ap, fmt);
    putchar(',');
    vprintf(fmt, ap);
    va_end(ap);
  }
  printf("}\n");
  fflush(stdout);
  pthread_mutex_unlock(&g_out_mu);
}

/* Publishes g_st (caller holds g_st_mu). */
static void publish(void) {
  procctl_state_set(&g_ctx, &g_st, sizeof g_st, 0);
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
  /* After ") " comes field 3; field 22 is 19 fields further. */
  int field = 2;
  for (; *p && field < 22; p++)
    if (*p == ' ') field++;
  return strtoull(p, NULL, 10);
}

/* ---- exclusive resource: the reference holder loop (SPEC §9.1) ------------ */

static void report(uint32_t state, uint64_t grant, uint64_t seen) {
  procctl_resource_report(&g_ctx, TESTCLIENT_KIND_TOKEN, g_token_id, state, grant,
                          PROCCTL_RES_F_WANT, 0, seen, 0);
}

static void *control_thread(void *arg) {
  (void)arg;
  sigset_t all;
  sigfillset(&all);
  pthread_sigmask(SIG_BLOCK, &all, NULL);
  pthread_setname_np(pthread_self(), "tc-ctl");
  uint64_t held = 0;
  report(PROCCTL_RES_IDLE, 0, 0);
  for (;;) {
    procctl_grant_t g;
    if (procctl_grant_get(&g_ctx, TESTCLIENT_KIND_TOKEN, g_token_id, &g) != 0) {
      procctl_wait(&g_ctx, 10000000ull);
      continue;
    }
    bool ok = g.grant_id && procctl_grant_usable(&g_ctx, &g, 0) && !procctl_lease_expired(&g_ctx, 0);
    if (held && (!ok || g.grant_id != held)) {
      if (ok) { /* renewal: transfer */
        held = g.grant_id;
        report(PROCCTL_RES_HOLDING, held, g.seen_gen);
      } else {
        logj("release", "\"grant\":%llu", (unsigned long long)held);
        pthread_mutex_lock(&g_st_mu);
        g_st.releases++;
        g_st.holding = 0;
        publish();
        pthread_mutex_unlock(&g_st_mu);
        report(PROCCTL_RES_RELEASED, held, g.seen_gen);
        held = 0;
      }
    } else if (!held && ok) {
      report(PROCCTL_RES_ACQUIRING, g.grant_id, g.seen_gen);
      logj("acquire", "\"grant\":%llu", (unsigned long long)g.grant_id);
      usleep((useconds_t)g_acquire_ms * 1000);
      held = g.grant_id;
      pthread_mutex_lock(&g_st_mu);
      g_st.acquires++;
      g_st.holding = 1;
      publish();
      pthread_mutex_unlock(&g_st_mu);
      report(PROCCTL_RES_HOLDING, held, g.seen_gen);
      if (g_holder_hang) {
        logj("hang", "\"grant\":%llu", (unsigned long long)held);
        for (;;) pause(); /* holds forever: the arbiter must never re-grant */
      }
    } else {
      report(held ? PROCCTL_RES_HOLDING : PROCCTL_RES_IDLE, held, g.seen_gen);
    }
    procctl_wait(&g_ctx, 50000000ull);
  }
  return NULL;
}

/* ---- hostile regions (S8) ---------------------------------------------------- */

/* Builds a region by hand, like procctl_init, with one thing wrong, and keeps
 * it open. The agent must reject it (or, for the state modes, isolate it) and
 * stay healthy. */
static _Alignas(8) unsigned char g_hstate[64 + ((sizeof(struct testclient_status) + 7) & ~7ul)];

static int hostile(const char *mode) {
  long P = sysconf(_SC_PAGESIZE);
  size_t area = (size_t)P, total = (size_t)P + area;
  static const char *const modes[] = {"no-seals",  "grow-seal-missing", "write-sealed",   "wrong-size",
                                      "spoof-name", "spoof-pid",         "bad-crc",        "bad-offsets",
                                      "huge-len",   "bad-state-addr",    "state-never-even", "no-state"};
  int known = 0;
  for (size_t i = 0; i < sizeof modes / sizeof *modes; i++) known |= !strcmp(mode, modes[i]);
  if (!known) {
    fprintf(stderr, "unknown hostile mode %s\n", mode);
    return 2;
  }
#define MODE(m) (!strcmp(mode, m))
  const char *name = "procctl:" TESTCLIENT_CONSUMER "-v1";
  int fd = (int)syscall(SYS_memfd_create, name, MFD_CLOEXEC | MFD_ALLOW_SEALING);
  if (fd < 0 || ftruncate(fd, (off_t)total) != 0) {
    logj("hostile_failed", "\"errno\":%d", errno);
    return 1;
  }
  /* A valid state block carrying a valid status, so accepted modes report
   * who they are. */
  struct procctl_state_hdr sh;
  memset(&sh, 0, sizeof sh);
  uint64_t inst = procctl_now_ns() | 1;
  uint32_t state_len = (uint32_t)sizeof g_st;
  memcpy(sh.magic, "PROCSTAT", 8);
  sh.instance_id = inst;
  sh.writer_pid = (uint32_t)getpid();
  sh.state_len = state_len;
  memcpy(g_hstate, &sh, sizeof sh);
  memcpy(g_hstate + sizeof sh, &g_st, sizeof g_st);
  ((struct procctl_state_hdr *)g_hstate)->crc32c = procctl__crc32c(g_hstate, sizeof sh + state_len);
  ((struct procctl_state_hdr *)g_hstate)->seq = MODE("state-never-even") ? 3 : 2;

  struct procctl_wire_header h;
  memset(&h, 0, sizeof h);
  memcpy(h.signature, "PROC_CTL", 8);
  h.spec_major = PROCCTL_SPEC_MAJOR;
  h.spec_minor = PROCCTL_SPEC_MINOR;
  h.header_size = (uint32_t)sizeof h;
  h.total_size = MODE("wrong-size") ? total + (size_t)P : total;
  h.page_size = (uint32_t)P;
  memcpy(h.consumer, MODE("spoof-name") ? "other" : TESTCLIENT_CONSUMER,
         strlen(MODE("spoof-name") ? "other" : TESTCLIENT_CONSUMER));
  h.schema_version = TESTCLIENT_SCHEMA_VERSION;
  h.creator_pid = MODE("spoof-pid") ? 4242 : (uint32_t)getpid(); /* informational only */
  h.agent_area_off = MODE("bad-offsets") ? 2 * (uint64_t)P : (uint64_t)P;
  h.agent_area_size = area;
  h.state_addr = MODE("bad-state-addr") ? 0x10000 : MODE("no-state") ? 0 : (uint64_t)(uintptr_t)g_hstate;
  h.state_size = MODE("huge-len") ? 1u << 20 : (uint32_t)sizeof g_hstate;
  h.created_at_ns = procctl_now_ns();
  h.instance_id = inst;
  h.header_crc32c = procctl__crc32c(&h, offsetof(struct procctl_wire_header, header_crc32c));
  if (MODE("bad-crc")) h.header_crc32c ^= 1;
  if (pwrite(fd, &h, sizeof h, 0) != (ssize_t)sizeof h) return 1;

  int seals = F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL;
  if (MODE("grow-seal-missing")) seals &= ~F_SEAL_GROW;
  if (MODE("write-sealed")) seals |= F_SEAL_WRITE;
  if (!MODE("no-seals") && fcntl(fd, F_ADD_SEALS, seals) != 0) {
    logj("hostile_failed", "\"errno\":%d", errno);
    return 1;
  }
#undef MODE
  logj("hostile", "\"mode\":\"%s\",\"fd\":%d", mode, fd);
  for (;;) pause();
}

/* ---- noise (S9) ----------------------------------------------------------------- */

/* N idle processes holding M fds each (scan cost), plus an exec storm of R
 * short-lived execs per second (mapping events). No procctl region. */
static int noise(long procs, long fds, long exec_rate, const char *self) {
  for (long i = 0; i < procs; i++) {
    pid_t c = fork();
    if (c == 0) {
      prctl(PR_SET_PDEATHSIG, SIGKILL);
      for (long k = 0; k < fds; k++)
        if (dup(0) < 0) break;
      for (;;) pause();
    }
    if (c < 0) {
      logj("noise_failed", "\"errno\":%d,\"procs\":%ld", errno, i);
      break;
    }
  }
  logj("noise", "\"procs\":%ld,\"fds\":%ld,\"exec_rate\":%ld", procs, fds, exec_rate);
  uint64_t execs = 0, last_log = procctl_now_ns();
  for (;;) {
    if (exec_rate <= 0) {
      pause();
      continue;
    }
    pid_t c = fork();
    if (c == 0) {
      execl(self, self, "--noop", (char *)NULL);
      _exit(127);
    }
    if (c > 0) execs++;
    while (waitpid(-1, NULL, WNOHANG) > 0) {
    }
    usleep((useconds_t)(1000000 / exec_rate));
    uint64_t now = procctl_now_ns();
    if (now - last_log > 10000000000ull) {
      logj("execs", "\"total\":%llu", (unsigned long long)execs);
      last_log = now;
    }
  }
}

/* ---- main loop: poll, verify, ack ------------------------------------------- */

static int verify(const uint8_t *b, int n, uint64_t *tag, uint64_t *write_ns) {
  struct testclient_payload h;
  if ((size_t)n < sizeof h) return 0;
  memcpy(&h, b, sizeof h);
  if (sizeof h + h.pattern_len != (size_t)n) return 0;
  if (procctl__crc32c(b + sizeof h, h.pattern_len) != h.pattern_crc32c) return 0;
  for (uint32_t i = 0; i < h.pattern_len; i++)
    if (b[sizeof h + i] != testclient_pattern(h.seq_tag, i)) return 0;
  *tag = h.seq_tag;
  *write_ns = h.agent_write_ns;
  return 1;
}

int main(int argc, char **argv) {
  long poll_us = 1000, exit_after_s = 0, init_delay_ms = 0, fork_after_ms = 0, fork_workers = 1;
  int exclusive = 0, fork_reinit = 0;
  long noise_procs = 0, noise_fds = 0, exec_rate = 0;
  const char *hostile_mode = NULL;
  for (int i = 1; i < argc; i++) {
    /* Accept both "--opt value" and "--opt=value". */
    char name[64];
    const char *val = NULL;
    const char *eq = strchr(argv[i], '=');
    if (eq && (size_t)(eq - argv[i]) < sizeof name) {
      memcpy(name, argv[i], (size_t)(eq - argv[i]));
      name[eq - argv[i]] = 0;
      val = eq + 1;
    } else {
      snprintf(name, sizeof name, "%s", argv[i]);
    }
#define NEXT() (val ? val : (i + 1 < argc ? argv[++i] : "0"))
    if (!strcmp(name, "--poll-us")) poll_us = atol(NEXT());
    else if (!strcmp(name, "--exclusive")) exclusive = 1;
    else if (!strcmp(name, "--acquire-ms")) g_acquire_ms = atol(NEXT());
    else if (!strcmp(name, "--exit-after-s")) exit_after_s = atol(NEXT());
    else if (!strcmp(name, "--init-delay-ms")) init_delay_ms = atol(NEXT());
    else if (!strcmp(name, "--fork-after-ms")) fork_after_ms = atol(NEXT());
    else if (!strcmp(name, "--fork-workers")) fork_workers = atol(NEXT());
    else if (!strcmp(name, "--fork-reinit")) fork_reinit = 1;
    else if (!strcmp(name, "--holder-hang")) g_holder_hang = 1;
    else if (!strcmp(name, "--hostile")) hostile_mode = NEXT();
    else if (!strcmp(name, "--noise-procs")) noise_procs = atol(NEXT());
    else if (!strcmp(name, "--noise-fds")) noise_fds = atol(NEXT());
    else if (!strcmp(name, "--exec-rate")) exec_rate = atol(NEXT());
    else if (!strcmp(name, "--noop")) return 0;
    else {
      fprintf(stderr, "usage: %s [--poll-us N] [--exclusive [--acquire-ms N] [--holder-hang]] [--exit-after-s N]"
              " [--init-delay-ms N] [--fork-after-ms N [--fork-workers K] [--fork-reinit]]\n"
              "       %s --hostile=MODE\n"
              "       %s --noise-procs=N [--noise-fds=M] [--exec-rate=R]\n",
              argv[0], argv[0], argv[0]);
      return 2;
    }
#undef NEXT
  }
  const char *pod = getenv("POD_NAME");
  if (!pod) pod = "local";
  strncpy((char *)g_st.pod, pod, sizeof g_st.pod - 1);
  g_st.ns_pid = getpid();
  g_st.start_time = self_start_time();
  g_st.mode = exclusive ? TESTCLIENT_MODE_EXCLUSIVE : 0;
  memset(g_token_id, 0x77, sizeof g_token_id); /* one node-wide token */
  if (hostile_mode) return hostile(hostile_mode);
  if (noise_procs > 0 || exec_rate > 0) return noise(noise_procs, noise_fds, exec_rate, "/proc/self/exe");

  /* A library that initializes on first use (e.g. on the first CUDA call). */
  uint64_t t_exec = procctl_now_ns();
  if (init_delay_ms > 0) usleep((useconds_t)init_delay_ms * 1000);

  procctl_config_t cfg = {.consumer = TESTCLIENT_CONSUMER,
                          .schema_version = TESTCLIENT_SCHEMA_VERSION,
                          .payload_capacity = PAYLOAD_CAP,
                          .resource_slots = SLOTS,
                          .state_capacity = sizeof(struct testclient_status),
                          .state_storage = g_storage};
  int r = procctl_init(&g_ctx, &cfg);
  if (r) {
    logj("init_failed", "\"err\":%d", r);
    return 1;
  }
  uint64_t start = procctl_now_ns();
  logj("start", "\"start_time\":%llu,\"exclusive\":%d,\"init_delay_ms\":%ld",
       (unsigned long long)g_st.start_time, exclusive, init_delay_ms);
  (void)t_exec;
  pthread_mutex_lock(&g_st_mu);
  publish();
  pthread_mutex_unlock(&g_st_mu);

  if (exclusive) {
    pthread_t th;
    pthread_create(&th, NULL, control_thread, NULL);
  }

  static uint8_t buf[PAYLOAD_CAP];
  int was_expired = -1, first = 1, forked = 0;
  uint64_t last_publish = 0;
  for (;;) {
    uint64_t now = procctl_now_ns();
    if (exit_after_s && now - start > (uint64_t)exit_after_s * 1000000000ull) break;
    if (!forked && fork_after_ms > 0 && now - start > (uint64_t)fork_after_ms * 1000000ull) {
      forked = 1;
      /* e.g. dataloader workers: children that keep running as clients. */
      for (long k = 0; k < fork_workers; k++) {
        /* Hold g_st_mu across fork so the child can't inherit it locked by
         * the control thread (which doesn't exist in the child). */
        pthread_mutex_lock(&g_st_mu);
        pid_t c = fork();
        if (c == 0) {
          char parent[64];
          snprintf(parent, sizeof parent, "%s", (const char *)g_st.pod);
          int ack = procctl_ack(&g_ctx, 1, 0); /* must be refused: -ECHILD */
          if (fork_reinit) {
            int rr = procctl_reinit(&g_ctx); /* own region; fires hello */
            snprintf((char *)g_st.pod, sizeof g_st.pod, "%.48s-child%ld", parent, k % 1000);
            g_st.ns_pid = getpid();
            g_st.start_time = self_start_time();
            g_st.last_gen = 0;
            publish();
            pthread_mutex_unlock(&g_st_mu);
            logj("forked_child", "\"reinit\":%d,\"inherited_ack\":%d", rr, ack);
          } else {
            pthread_mutex_unlock(&g_st_mu);
            logj("forked_child", "\"reinit\":-1,\"inherited_ack\":%d", ack);
          }
          first = 1;
          was_expired = -1;
          start = procctl_now_ns();
          break;
        }
        pthread_mutex_unlock(&g_st_mu);
        logj("forked", "\"child\":%d", c);
      }
    }
    uint64_t gen = 0;
    int n = procctl_poll(&g_ctx, buf, sizeof buf, &gen);
    pthread_mutex_lock(&g_st_mu);
    g_st.polls++;
    bool dirty = false;
    if (n > 0) {
      uint64_t tag = 0, wns = 0;
      uint64_t recv = procctl_now_ns();
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
      dirty = true;
    } else if (n == -EAGAIN) {
      g_st.busy++;
    }
    bool expired = procctl_lease_expired(&g_ctx, now);
    if ((int)expired != was_expired) {
      if (was_expired != -1) {
        if (expired) g_st.lease_expired_at_ns = now;
        else g_st.lease_restored_at_ns = now;
      }
      g_st.lease_expired = expired;
      logj("lease", "\"expired\":%d", expired ? 1 : 0);
      was_expired = expired;
      dirty = true;
    }
    /* Rate-limit state updates (SPEC §7.5): on change, at most every 5 ms. */
    if (dirty && now - last_publish > 5000000ull) {
      publish();
      last_publish = now;
      dirty = false;
    }
    pthread_mutex_unlock(&g_st_mu);
    if (n > 0) procctl_ack(&g_ctx, gen, PROCCTL_TRY);
    usleep((useconds_t)poll_us);
  }
  logj("exit", "\"torn\":%llu,\"changes\":%llu", (unsigned long long)g_st.torn,
       (unsigned long long)g_st.changes);
  return 0;
}
