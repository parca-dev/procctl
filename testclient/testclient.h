/* Wire structs of the "testclient" consumer: the payload the test agent
 * sends and the status the test client reports. The test agent's Go types are
 * generated from this header with `go tool cgo -godefs` (scripts/gen-types.sh),
 * the same way any procctl consumer would do it.
 *
 * SPDX-License-Identifier: Apache-2.0 */
#ifndef TESTCLIENT_H
#define TESTCLIENT_H

#include <stdint.h>

#define TESTCLIENT_CONSUMER "testclient"
#define TESTCLIENT_SCHEMA_VERSION 1u
#define TESTCLIENT_KIND_TOKEN 1u /* the simulated exclusive resource */

/* Agent → client. Followed by pattern_len bytes of pattern(seq_tag), so the
 * client can detect a torn payload independently of procctl's own checks. */
struct testclient_payload {
  uint64_t seq_tag;        /* increments with every write the agent makes */
  uint64_t agent_write_ns; /* agent's CLOCK_BOOTTIME just before the write */
  uint32_t pattern_len;
  uint32_t pattern_crc32c;
};

/* Client → agent (consumer state). */
struct testclient_status {
  uint64_t last_seq_tag;         /* tag of the last payload applied */
  uint64_t last_gen;             /* its generation */
  uint64_t last_agent_write_ns;  /* its agent_write_ns */
  uint64_t last_recv_ns;         /* client's CLOCK_BOOTTIME when it read it */
  uint64_t torn;                 /* payloads that failed the pattern check */
  uint64_t backwards;            /* generations lower than one already applied */
  uint64_t redelivered;          /* the same generation again: lease recovery (SPEC §8.3) */
  uint64_t polls;
  uint64_t changes;
  uint64_t busy;
  uint64_t lease_expired_at_ns;  /* last expiry, 0 = never */
  uint64_t lease_restored_at_ns; /* last recovery, 0 = never */
  uint64_t start_time;           /* field 22 of /proc/self/stat */
  uint64_t acquires;             /* exclusive mode */
  uint64_t releases;
  int32_t ns_pid;                /* getpid() inside the pod */
  uint32_t lease_expired;        /* current lease state */
  uint32_t holding;              /* exclusive mode: holding the token now */
  uint32_t mode;                 /* TESTCLIENT_MODE_* bits */
  uint8_t pod[64];               /* POD_NAME, NUL-padded */
};

#define TESTCLIENT_MODE_EXCLUSIVE 1u
#define TESTCLIENT_MODE_HOSTILE 2u

/* The test pattern: shared by client and agent. */
static inline uint8_t testclient_pattern(uint64_t tag, uint32_t i) {
  uint64_t x = tag * 0x9e3779b97f4a7c15ull + i;
  x ^= x >> 31;
  x *= 0xbf58476d1ce4e5b9ull;
  return (uint8_t)(x >> 56);
}

#endif
