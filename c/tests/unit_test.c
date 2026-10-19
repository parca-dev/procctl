/* Unit tests for procctl.h. A minimal in-test "agent" writes the agent area
 * through a second, writable mapping of the region (as a real agent would via
 * pidfd_getfd) so every process-side API can be exercised in one process. */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <assert.h>
#include <fcntl.h>
#include <linux/futex.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond)                                                                    \
  do {                                                                                 \
    if (!(cond)) {                                                                     \
      fprintf(stderr, "%s:%d: CHECK failed: %s\n", __FILE__, __LINE__, #cond);         \
      exit(1);                                                                         \
    }                                                                                  \
  } while (0)

#define SLOTS 4
#define STATE_CAP 256
static _Alignas(8) uint8_t g_storage[PROCCTL_STATE_STORAGE(SLOTS, STATE_CAP)];
static procctl_t g_ctx;

/* ---- test agent ---------------------------------------------------------- */

struct agent {
  uint8_t *map;
  size_t size;
  uint8_t *area;
  uint16_t slots;
};

static void agent_open(struct agent *a, procctl_t *ctx) {
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)ctx->opaque;
  char path[64];
  snprintf(path, sizeof path, "/proc/self/fd/%d", c->fd);
  int fd = open(path, O_RDWR | O_CLOEXEC);
  CHECK(fd >= 0);
  struct stat st;
  CHECK(fstat(fd, &st) == 0);
  a->size = (size_t)st.st_size;
  a->map = mmap(NULL, a->size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  CHECK(a->map != MAP_FAILED);
  close(fd);
  const struct procctl_wire_header *h = (const struct procctl_wire_header *)(const void *)a->map;
  a->area = a->map + h->agent_area_off;
  a->slots = h->resource_slots;
}

struct grant { uint32_t kind; uint8_t id[16]; uint64_t grant_id, duration; };

/* Seqlock writer per SPEC §7.1 (single-threaded test: no CAS needed). */
static uint64_t agent_write(struct agent *a, const void *payload, uint32_t len, uint32_t flags,
                            const struct grant *g, int ng) {
  struct procctl_ctrl *cb = (struct procctl_ctrl *)(void *)a->area;
  uint64_t s = __atomic_load_n(&cb->seq, __ATOMIC_ACQUIRE);
  uint64_t t = (s & 1) ? s + 2 : s + 1;
  __atomic_store_n(&cb->seq, t, __ATOMIC_RELAXED);
  __atomic_thread_fence(__ATOMIC_RELEASE);
  struct procctl_grant_entry *table = (struct procctl_grant_entry *)(void *)(cb + 1);
  size_t gt = (size_t)a->slots * sizeof *table;
  memset(table, 0, gt);
  for (int i = 0; i < ng; i++) {
    memcpy(table[i].resource_id, g[i].id, 16);
    table[i].grant_id = g[i].grant_id ? g[i].grant_id : t + 1;
    table[i].duration_ns = g[i].duration;
    table[i].kind = g[i].kind;
  }
  uint8_t *body = (uint8_t *)table;
  memcpy(body + gt, payload, len);
  cb->payload_crc32c = procctl__crc32c(body, gt + len);
  cb->payload_len = len;
  cb->flags = flags;
  __atomic_store_n(&cb->seq, t + 1, __ATOMIC_RELEASE);
  syscall(SYS_futex, (uint32_t *)(void *)&cb->seq, FUTEX_WAKE, INT32_MAX, NULL, NULL, 0);
  return t + 1;
}

static void agent_set(struct agent *a, uint64_t writer, uint64_t hb, uint64_t lease) {
  struct procctl_ctrl *cb = (struct procctl_ctrl *)(void *)a->area;
  cb->writer_id = writer;
  cb->heartbeat_ns = hb;
  cb->lease_ns = lease;
}

/* ---- state block checks ---------------------------------------------------- */

#define SH ((const struct procctl_state_hdr *)(const void *)g_storage)
#define RES ((const struct procctl_res_entry *)(const void *)(g_storage + sizeof(struct procctl_state_hdr)))

static void check_state_consistent(void) {
  CHECK(memcmp(SH->magic, "PROCSTAT", 8) == 0);
  CHECK((SH->seq & 1) == 0);
  size_t used = sizeof *SH + SLOTS * sizeof *RES + SH->state_len;
  uint8_t copy[sizeof g_storage];
  memcpy(copy, g_storage, used);
  struct procctl_state_hdr *ch = (struct procctl_state_hdr *)(void *)copy;
  uint32_t want = ch->crc32c;
  ch->seq = 0;
  ch->crc32c = 0;
  CHECK(procctl__crc32c(copy, used) == want);
}

/* ---- tests ------------------------------------------------------------------- */

static void test_init_and_header(void) {
  procctl_config_t cfg = {.consumer = "unit_test", .schema_version = 7, .payload_capacity = 1000,
                          .resource_slots = SLOTS, .state_capacity = STATE_CAP,
                          .state_storage = g_storage};
  CHECK(procctl_init(&g_ctx, &cfg) == 0);
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)g_ctx.opaque;

  char path[64], link[128];
  snprintf(path, sizeof path, "/proc/self/fd/%d", c->fd);
  ssize_t n = readlink(path, link, sizeof link - 1);
  CHECK(n > 0);
  link[n] = 0;
  CHECK(strncmp(link, "/memfd:procctl:unit_test-v1", 27) == 0);

  int seals = fcntl(c->fd, F_GET_SEALS);
  CHECK((seals & (F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL)) ==
        (F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL));
  CHECK(!(seals & F_SEAL_WRITE));

  const struct procctl_wire_header *h = (const struct procctl_wire_header *)(const void *)c->map;
  CHECK(memcmp(h->signature, "PROC_CTL", 8) == 0);
  CHECK(procctl__crc32c(h, offsetof(struct procctl_wire_header, header_crc32c)) == h->header_crc32c);
  CHECK(h->state_addr == (uint64_t)(uintptr_t)g_storage);
  CHECK(h->state_size == PROCCTL_STATE_BLOCK_SIZE(SLOTS, STATE_CAP));
  CHECK(h->instance_id == SH->instance_id);
  check_state_consistent();

  /* Bad configs. */
  procctl_t x;
  procctl_config_t bad = cfg;
  bad.consumer = "Bad-Name";
  CHECK(procctl_init(&x, &bad) == -EINVAL);
  bad = cfg;
  bad.resource_slots = 65;
  CHECK(procctl_init(&x, &bad) == -EINVAL);
  printf("ok init_and_header\n");
}

static void test_poll_ack_lease(struct agent *a) {
  uint8_t buf[1000];
  uint64_t gen = 0;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == 0); /* never written */
  CHECK(procctl_lease_expired(&g_ctx, 0));                 /* no agent */
  CHECK(!procctl_agent_attached(&g_ctx));

  agent_set(a, 0xabc, 1000, 5000000000ull);
  const char *msg = "hello config";
  uint64_t g1 = agent_write(a, msg, (uint32_t)strlen(msg), 1, NULL, 0);
  CHECK(procctl_agent_attached(&g_ctx));
  int r = procctl_poll(&g_ctx, buf, sizeof buf, &gen);
  CHECK(r == (int)strlen(msg));
  CHECK(gen == g1);
  CHECK(memcmp(buf, msg, strlen(msg)) == 0);
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == 0); /* consumed */

  CHECK(procctl_ack(&g_ctx, g1, 0) == 0);
  CHECK(SH->applied_gen == g1);
  CHECK(SH->applied_writer_id == 0xabc);
  CHECK(procctl_ack(&g_ctx, g1 - 2, 0) == 0);
  CHECK(SH->applied_gen == g1); /* never decreases */
  check_state_consistent();

  /* Too small a buffer consumes the generation. */
  agent_write(a, msg, (uint32_t)strlen(msg), 1, NULL, 0);
  CHECK(procctl_poll(&g_ctx, buf, 4, &gen) == -E2BIG);
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == 0);

  /* PAYLOAD_VALID clear → -ENODATA. */
  agent_write(a, "", 0, 0, NULL, 0);
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == -ENODATA);

  /* Lease: heartbeat changes keep it alive; stale heartbeat expires it, and
   * recovery re-delivers the current payload. */
  uint64_t now = procctl_now_ns();
  CHECK(!procctl_lease_expired(&g_ctx, now));
  CHECK(procctl_lease_expired(&g_ctx, now + 6000000000ull));
  agent_set(a, 0xabc, 2000, 5000000000ull);
  CHECK(!procctl_lease_expired(&g_ctx, now + 6000000001ull));
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == -ENODATA); /* re-delivered */

  /* Torn payload (CRC mismatch) is rejected. */
  uint64_t g2 = agent_write(a, msg, (uint32_t)strlen(msg), 1, NULL, 0);
  a->area[sizeof(struct procctl_ctrl) + SLOTS * sizeof(struct procctl_grant_entry)] ^= 0xff;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == -EAGAIN);
  (void)g2;
  printf("ok poll_ack_lease\n");
}

static void test_grants(struct agent *a) {
  struct grant g = {.kind = 1, .duration = 50000000ull};
  memset(g.id, 0x42, 16);
  procctl_grant_t out;
  uint8_t other[16];
  memset(other, 0x43, 16);

  uint64_t gen = agent_write(a, "x", 1, 1, &g, 1);
  CHECK(procctl_grant_get(&g_ctx, 1, g.id, &out) == 0);
  CHECK(out.grant_id == gen && out.duration_ns == g.duration && out.seen_gen == gen);
  CHECK(procctl_grant_get(&g_ctx, 1, other, &out) == 0 && out.grant_id == 0);
  CHECK(procctl_grant_get(&g_ctx, 2, g.id, &out) == 0 && out.grant_id == 0);

  CHECK(procctl_grant_get(&g_ctx, 1, g.id, &out) == 0);
  uint64_t t0 = procctl_now_ns();
  CHECK(procctl_grant_usable(&g_ctx, &out, t0));
  CHECK(procctl_grant_usable(&g_ctx, &out, t0 + 49000000ull));
  CHECK(!procctl_grant_usable(&g_ctx, &out, t0 + 51000000ull)); /* duration elapsed */

  CHECK(procctl_resource_report(&g_ctx, 1, g.id, PROCCTL_RES_HOLDING, out.grant_id,
                                PROCCTL_RES_F_WANT, 0, out.seen_gen, 0) == 0);
  const struct procctl_res_entry *e = &RES[0];
  CHECK(memcmp(e->resource_id, g.id, 16) == 0);
  CHECK(e->grant_id == out.grant_id && e->kind == 1 && e->state == PROCCTL_RES_HOLDING);
  CHECK(e->last_activity_ns != 0); /* activity stamped */
  CHECK(SH->grants_seen_gen == out.seen_gen);
  CHECK(procctl_resource_report(&g_ctx, 1, g.id, PROCCTL_RES_RELEASED, out.grant_id, 0, 0,
                                out.seen_gen - 2, 0) == 0);
  CHECK(SH->grants_seen_gen == out.seen_gen); /* never decreases */
  CHECK(e->state == PROCCTL_RES_RELEASED);
  /* Fill all slots, then overflow. */
  for (int i = 1; i < SLOTS; i++) {
    uint8_t id[16];
    memset(id, i, 16);
    CHECK(procctl_resource_report(&g_ctx, 1, id, PROCCTL_RES_IDLE, 0, PROCCTL_RES_F_WANT, 0, 0, 0) == 0);
  }
  uint8_t id9[16];
  memset(id9, 9, 16);
  CHECK(procctl_resource_report(&g_ctx, 1, id9, PROCCTL_RES_IDLE, 0, 0, 0, 0, 0) == -ENOSPC);
  check_state_consistent();

  CHECK(procctl_state_set(&g_ctx, "status", 6, 0) == 0);
  CHECK(SH->state_len == 6);
  CHECK(procctl_state_set(&g_ctx, g_storage, STATE_CAP + 1, 0) == -E2BIG);
  check_state_consistent();
  printf("ok grants\n");
}

static struct agent *g_agent;
static void *waker(void *arg) {
  (void)arg;
  usleep(50000);
  agent_write(g_agent, "w", 1, 1, NULL, 0);
  return NULL;
}

static void test_wait(struct agent *a) {
  uint64_t t0 = procctl_now_ns();
  CHECK(procctl_wait(&g_ctx, 20000000ull) == -ETIMEDOUT);
  CHECK(procctl_now_ns() - t0 >= 19000000ull);

  g_agent = a;
  pthread_t th;
  pthread_create(&th, NULL, waker, NULL);
  t0 = procctl_now_ns();
  CHECK(procctl_wait(&g_ctx, 5000000000ull) == 0);
  uint64_t waited = procctl_now_ns() - t0;
  pthread_join(th, NULL);
  CHECK(waited < 1000000000ull); /* woken, not timed out */
  printf("ok wait (woken after %.1f ms)\n", waited / 1e6);
}

static void test_fork(void) {
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)g_ctx.opaque;
  int parent_fd = c->fd;
  uint64_t parent_instance = c->instance_id;
  pid_t pid = fork();
  CHECK(pid >= 0);
  if (pid == 0) {
    /* Inherited block is stale: writes refused. Reading still works. */
    uint8_t buf[64];
    uint64_t gen;
    if (procctl_ack(&g_ctx, 1, 0) != -ECHILD) _exit(10);
    if (procctl_state_set(&g_ctx, "x", 1, 0) != -ECHILD) _exit(11);
    if (procctl_poll(&g_ctx, buf, sizeof buf, &gen) < -100) _exit(12);
    if (procctl_reinit(&g_ctx) != 0) _exit(13);
    if (c->fd == parent_fd && c->instance_id == parent_instance) _exit(14);
    if (c->instance_id == parent_instance) _exit(15);
    if (procctl_state_set(&g_ctx, "child", 5, 0) != 0) _exit(16);
    if (SH->writer_pid != (uint32_t)getpid()) _exit(17);
    _exit(0);
  }
  int status = 0;
  CHECK(waitpid(pid, &status, 0) == pid);
  if (!WIFEXITED(status) || WEXITSTATUS(status) != 0)
    fprintf(stderr, "child exit status %d\n", WEXITSTATUS(status));
  CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);
  CHECK(SH->writer_pid == (uint32_t)getpid()); /* parent's block untouched */
  printf("ok fork\n");
}

static void on_segv(int sig) {
  (void)sig;
  _exit(42);
}

static void test_readonly_mapping(void) {
  struct procctl__ctx *c = (struct procctl__ctx *)(void *)g_ctx.opaque;
  pid_t pid = fork();
  if (pid == 0) {
    /* Our own handler, so sanitizers' SEGV handlers don't change the outcome. */
    struct sigaction sa;
    memset(&sa, 0, sizeof sa);
    sa.sa_handler = on_segv;
    sigaction(SIGSEGV, &sa, NULL);
    ((volatile uint8_t *)c->area)[200] = 1; /* must fault */
    _exit(0);
  }
  int status = 0;
  waitpid(pid, &status, 0);
  CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 42);
  printf("ok readonly_mapping\n");
}

int main(void) {
  test_init_and_header();
  struct agent a;
  agent_open(&a, &g_ctx);
  test_poll_ack_lease(&a);
  test_grants(&a);
  test_wait(&a);
  test_readonly_mapping();
  test_fork();
  procctl_close(&g_ctx);
  printf("all ok\n");
  return 0;
}
