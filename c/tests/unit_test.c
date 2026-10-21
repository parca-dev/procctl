/* Unit tests for procctl.h. A minimal in-test "agent" writes the agent area
 * through a second, writable mapping of the region (as a real agent does via
 * /proc/<pid>/fd), so every process-side call can be exercised in one process. */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define CHECK(cond)                                                                    \
  do {                                                                                 \
    if (!(cond)) {                                                                     \
      fprintf(stderr, "%s:%d: CHECK failed: %s\n", __FILE__, __LINE__, #cond);         \
      exit(1);                                                                         \
    }                                                                                  \
  } while (0)

#define STATE_CAP 256
static _Alignas(8) uint8_t g_storage[PROCCTL_STATE_SIZE(STATE_CAP)];
static procctl_t g_ctx;
#define CTX ((struct procctl__ctx *)(void *)g_ctx.opaque)
#define ST ((const struct procctl_state *)(const void *)g_storage)

/* ---- test agent ---------------------------------------------------------- */

struct agent {
  uint8_t *map;
  struct procctl_ctrl *cb;
};

static void agent_open(struct agent *a) {
  char path[64];
  snprintf(path, sizeof path, "/proc/self/fd/%d", CTX->fd);
  int fd = open(path, O_RDWR | O_CLOEXEC);
  CHECK(fd >= 0);
  struct stat st;
  CHECK(fstat(fd, &st) == 0);
  a->map = mmap(NULL, (size_t)st.st_size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
  CHECK(a->map != MAP_FAILED);
  close(fd);
  a->cb = (struct procctl_ctrl *)(void *)(a->map + ((struct procctl_header *)(void *)a->map)->page_size);
}

/* Seqlock writer (single-threaded test: plain stores suffice). */
static uint64_t agent_write(struct agent *a, const void *payload, uint32_t len) {
  uint64_t s = a->cb->seq | 1u;
  __atomic_store_n(&a->cb->seq, s, __ATOMIC_RELAXED);
  __atomic_thread_fence(__ATOMIC_RELEASE);
  memcpy((uint8_t *)(a->cb + 1), payload, len);
  a->cb->payload_len = len;
  a->cb->payload_crc = pc__crc32c((const uint8_t *)payload, len);
  __atomic_store_n(&a->cb->seq, s + 1, __ATOMIC_RELEASE);
  return s + 1;
}

/* ---- tests ------------------------------------------------------------------- */

static void test_init(void) {
  procctl_config_t cfg = {.consumer = "unit_test", .schema_version = 7, .payload_capacity = 1000,
                          .state_capacity = STATE_CAP, .state_storage = g_storage};
  CHECK(procctl_init(&g_ctx, &cfg) == 0);

  char path[64], link[128];
  snprintf(path, sizeof path, "/proc/self/fd/%d", CTX->fd);
  ssize_t n = readlink(path, link, sizeof link - 1);
  CHECK(n > 0);
  link[n] = 0;
  CHECK(strncmp(link, "/memfd:procctl:unit_test-v1", 27) == 0);

  int seals = fcntl(CTX->fd, F_GET_SEALS);
  CHECK((seals & (F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL)) == (F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL));
  CHECK(!(seals & F_SEAL_WRITE));

  const struct procctl_header *h = (const struct procctl_header *)(const void *)CTX->map;
  CHECK(memcmp(h->magic, "PROC_CTL", 8) == 0 && h->version == PROCCTL_VERSION);
  CHECK(h->schema_version == 7 && strcmp((const char *)h->consumer, "unit_test") == 0);
  CHECK(h->state_addr == (uint64_t)(uintptr_t)g_storage);
  CHECK(h->state_size == PROCCTL_STATE_SIZE(STATE_CAP));
  CHECK(h->instance_id != 0 && h->instance_id == ST->instance_id);
  CHECK(memcmp(ST->magic, "PROCSTAT", 8) == 0 && ST->seq == 2 && ST->pid == (uint32_t)getpid());

  procctl_t x;
  procctl_config_t bad = cfg;
  bad.consumer = "Bad-Name";
  CHECK(procctl_init(&x, &bad) == -EINVAL);
  bad = cfg;
  bad.state_storage = g_storage + 1;
  CHECK(procctl_init(&x, &bad) == -EINVAL);
  printf("ok init\n");
}

static void test_poll_ack_lease(struct agent *a) {
  uint8_t buf[1000];
  uint64_t gen = 0;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == 0); /* never written */
  CHECK(procctl_lease_expired(&g_ctx, 0));                 /* no owner */

  a->cb->owner_id = 0xabc;
  a->cb->heartbeat_ns = 1000;
  a->cb->lease_ns = 5000000000ull;
  const char *msg = "hello config";
  uint64_t g1 = agent_write(a, msg, (uint32_t)strlen(msg));
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == (int)strlen(msg));
  CHECK(gen == g1 && memcmp(buf, msg, strlen(msg)) == 0);
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == 0); /* consumed */

  uint64_t seq = ST->seq;
  CHECK(procctl_ack(&g_ctx, g1) == 0);
  CHECK(ST->applied_gen == g1 && ST->seq == seq + 2);
  CHECK(procctl_ack(&g_ctx, g1 - 2) == 0 && ST->applied_gen == g1); /* never decreases */

  CHECK(procctl_state_set(&g_ctx, "status", 6) == 0);
  CHECK(ST->len == 6 && memcmp(g_storage + sizeof *ST, "status", 6) == 0);
  CHECK(procctl_state_set(&g_ctx, buf, STATE_CAP + 1) == -E2BIG);

  /* Too small a buffer skips the generation. */
  agent_write(a, msg, (uint32_t)strlen(msg));
  CHECK(procctl_poll(&g_ctx, buf, 4, &gen) == -E2BIG);
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == 0);

  /* No payload: use defaults. */
  agent_write(a, "", 0);
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == -ENODATA);

  /* Torn payload: rejected, and retried once whole. */
  uint64_t g2 = agent_write(a, msg, (uint32_t)strlen(msg));
  ((uint8_t *)(a->cb + 1))[0] ^= 0xff;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == -EAGAIN);
  ((uint8_t *)(a->cb + 1))[0] ^= 0xff;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == (int)strlen(msg) && gen == g2);

  /* Mid-write (odd seq): busy. */
  a->cb->seq |= 1u;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == -EAGAIN);
  a->cb->seq += 1;
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == (int)strlen(msg));

  /* Lease: a changing heartbeat keeps it; a stale one expires it, and recovery
   * delivers the current configuration again. */
  uint64_t now = procctl_now_ns();
  CHECK(!procctl_lease_expired(&g_ctx, now));
  CHECK(procctl_lease_expired(&g_ctx, now + 6000000000ull));
  a->cb->heartbeat_ns = 2000;
  CHECK(!procctl_lease_expired(&g_ctx, now + 6000000001ull));
  CHECK(procctl_poll(&g_ctx, buf, sizeof buf, &gen) == (int)strlen(msg)); /* re-delivered */
  a->cb->owner_id = 0; /* uninstalled */
  CHECK(procctl_lease_expired(&g_ctx, now + 6000000002ull));
  printf("ok poll_ack_lease\n");
}

static void test_fork(void) {
  int parent_fd = CTX->fd;
  uint64_t parent_instance = ST->instance_id;
  pid_t pid = fork();
  CHECK(pid >= 0);
  if (pid == 0) {
    /* The inherited block is the parent's: writes are refused. */
    if (procctl_ack(&g_ctx, 1) != -ECHILD) _exit(10);
    if (procctl_state_set(&g_ctx, "x", 1) != -ECHILD) _exit(11);
    if (procctl_reinit(&g_ctx) != 0) _exit(12);
    if (CTX->fd == parent_fd && ST->instance_id == parent_instance) _exit(13);
    if (procctl_state_set(&g_ctx, "child", 5) != 0) _exit(14);
    if (ST->pid != (uint32_t)getpid()) _exit(15);
    _exit(0);
  }
  int status = 0;
  CHECK(waitpid(pid, &status, 0) == pid);
  CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);
  CHECK(ST->pid == (uint32_t)getpid()); /* the parent's block is untouched */
  printf("ok fork\n");
}

static void on_segv(int sig) {
  (void)sig;
  _exit(42);
}

static void test_readonly_mapping(void) {
  pid_t pid = fork();
  if (pid == 0) {
    /* Our own handler, so sanitizers' SEGV handlers don't change the outcome. */
    struct sigaction sa;
    memset(&sa, 0, sizeof sa);
    sa.sa_handler = on_segv;
    sigaction(SIGSEGV, &sa, NULL);
    ((volatile uint8_t *)CTX->map)[CTX->map_size - 1] = 1; /* must fault */
    _exit(0);
  }
  int status = 0;
  waitpid(pid, &status, 0);
  CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 42);
  printf("ok readonly_mapping\n");
}

int main(void) {
  test_init();
  struct agent a;
  agent_open(&a);
  test_poll_ack_lease(&a);
  test_readonly_mapping();
  test_fork();
  procctl_close(&g_ctx);
  printf("all ok\n");
  return 0;
}
