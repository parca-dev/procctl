/* stress_client: hammers procctl_poll, verifying every
 * payload it receives against a self-checking pattern, while a second thread
 * publishes a self-checking consumer state as fast as it can. Exits on stdin
 * EOF and prints JSON stats; exit status 1 if anything torn got through. */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <errno.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#define PAYLOAD_CAP 16384
#define STATE_CAP 1024
static _Alignas(8) unsigned char g_state[PROCCTL_STATE_SIZE(STATE_CAP)];
static procctl_t g_ctx;
static atomic_int g_stop;

/* Payload: u64 tag | u32 len | pattern(tag, len). */
static uint8_t pat(uint64_t tag, size_t i) {
  uint64_t x = tag * 0x9e3779b97f4a7c15ull + i;
  x ^= x >> 31;
  x *= 0xbf58476d1ce4e5b9ull;
  return (uint8_t)(x >> 56);
}

static int check_payload(const uint8_t *b, int n) {
  if (n < 12) return 0;
  uint64_t tag;
  uint32_t len;
  memcpy(&tag, b, 8);
  memcpy(&len, b + 8, 4);
  if (len != (uint32_t)n) return 0;
  for (int i = 12; i < n; i++)
    if (b[i] != pat(tag, (size_t)i)) return 0;
  return 1;
}

static void *state_writer(void *arg) {
  (void)arg;
  uint8_t buf[STATE_CAP];
  for (uint64_t n = 1; !atomic_load(&g_stop); n++) {
    size_t len = 16 + (size_t)(n % (STATE_CAP - 16));
    memcpy(buf, &n, 8);
    uint64_t l64 = len;
    memcpy(buf + 8, &l64, 8);
    for (size_t i = 16; i < len; i++) buf[i] = pat(n, i);
    procctl_state_set(&g_ctx, buf, len);
    if (n % 4) usleep((useconds_t)(n % 50)); /* bursts with short gaps */
  }
  return NULL;
}

static void *stdin_watch(void *arg) {
  (void)arg;
  char b[64];
  while (read(0, b, sizeof b) > 0) {
  }
  atomic_store(&g_stop, 1);
  return NULL;
}

int main(void) {
  procctl_config_t cfg = {.consumer = "stress", .schema_version = 1,
                          .payload_capacity = PAYLOAD_CAP,
                          .state_capacity = STATE_CAP, .state_storage = g_state};
  if (procctl_init(&g_ctx, &cfg)) return 2;
  pthread_t w, s;
  pthread_create(&w, NULL, state_writer, NULL);
  pthread_create(&s, NULL, stdin_watch, NULL);
  printf("{\"ev\":\"ready\"}\n");
  fflush(stdout);

  static uint8_t buf[PAYLOAD_CAP];
  uint64_t polls = 0, changes = 0, busy = 0, bad = 0, last_gen = 0, backwards = 0;
  while (!atomic_load(&g_stop)) {
    uint64_t gen = 0;
    int n = procctl_poll(&g_ctx, buf, sizeof buf, &gen);
    polls++;
    if (n > 0) {
      changes++;
      if (gen <= last_gen) backwards++;
      last_gen = gen;
      if (!check_payload(buf, n)) bad++;
      procctl_ack(&g_ctx, gen);
    } else if (n == -EAGAIN) {
      busy++;
    }
  }
  atomic_store(&g_stop, 1);
  pthread_join(w, NULL);
  printf("{\"ev\":\"stats\",\"polls\":%llu,\"changes\":%llu,\"busy\":%llu,\"bad\":%llu,"
         "\"backwards\":%llu}\n",
         (unsigned long long)polls, (unsigned long long)changes, (unsigned long long)busy,
         (unsigned long long)bad, (unsigned long long)backwards);
  fflush(stdout);
  return (bad || backwards) ? 1 : 0;
}
