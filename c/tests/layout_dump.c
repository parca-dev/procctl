/* Prints procctl.h's layout constants as JSON, for the Go conformance test. */
#define _GNU_SOURCE
#define PROCCTL_IMPLEMENTATION
#include "../procctl.h"

#include <stddef.h>
#include <stdio.h>

#define F(name, val) printf("%s\"%s\":%d", first ? "" : ",", name, (int)(val)), first = 0

int main(void) {
  int first = 1;
  printf("{");
  F("HEADER_SIZE", PROCCTL_HEADER_SIZE); F("CTRL_SIZE", PROCCTL_CTRL_SIZE);
  F("ENTRY_SIZE", PROCCTL_ENTRY_SIZE); F("STATE_HDR_SIZE", PROCCTL_STATE_HDR_SIZE);
  F("MAX_SLOTS", PROCCTL_MAX_SLOTS); F("MAX_STATE_SIZE", PROCCTL_MAX_STATE_SIZE);
  F("SPEC_MAJOR", PROCCTL_SPEC_MAJOR); F("SPEC_MINOR", PROCCTL_SPEC_MINOR);
  F("H_SIG", offsetof(struct procctl_wire_header, signature)); F("H_MAJOR", offsetof(struct procctl_wire_header, spec_major)); F("H_MINOR", offsetof(struct procctl_wire_header, spec_minor));
  F("H_HSIZE", offsetof(struct procctl_wire_header, header_size)); F("H_TOTAL", offsetof(struct procctl_wire_header, total_size)); F("H_PAGE", offsetof(struct procctl_wire_header, page_size));
  F("H_CONSUMER", offsetof(struct procctl_wire_header, consumer)); F("H_SCHEMA", offsetof(struct procctl_wire_header, schema_version)); F("H_CREATOR", offsetof(struct procctl_wire_header, creator_pid));
  F("H_AREA_OFF", offsetof(struct procctl_wire_header, agent_area_off)); F("H_AREA_SIZE", offsetof(struct procctl_wire_header, agent_area_size));
  F("H_STATE_ADDR", offsetof(struct procctl_wire_header, state_addr)); F("H_STATE_SIZE", offsetof(struct procctl_wire_header, state_size));
  F("H_SLOTS", offsetof(struct procctl_wire_header, resource_slots)); F("H_CREATED", offsetof(struct procctl_wire_header, created_at_ns)); F("H_INSTANCE", offsetof(struct procctl_wire_header, instance_id));
  F("H_CRC", offsetof(struct procctl_wire_header, header_crc32c));
  F("C_SEQ", offsetof(struct procctl_ctrl, seq)); F("C_LEN", offsetof(struct procctl_ctrl, payload_len)); F("C_FLAGS", offsetof(struct procctl_ctrl, flags));
  F("C_WRITER", offsetof(struct procctl_ctrl, writer_id)); F("C_HB", offsetof(struct procctl_ctrl, heartbeat_ns)); F("C_LEASE", offsetof(struct procctl_ctrl, lease_ns));
  F("C_CRC", offsetof(struct procctl_ctrl, payload_crc32c));
  F("S_MAGIC", offsetof(struct procctl_state_hdr, magic)); F("S_SEQ", offsetof(struct procctl_state_hdr, seq)); F("S_INSTANCE", offsetof(struct procctl_state_hdr, instance_id));
  F("S_PID", offsetof(struct procctl_state_hdr, writer_pid)); F("S_LEN", offsetof(struct procctl_state_hdr, state_len)); F("S_APPLIED", offsetof(struct procctl_state_hdr, applied_gen));
  F("S_APPLIED_W", offsetof(struct procctl_state_hdr, applied_writer_id)); F("S_SEEN", offsetof(struct procctl_state_hdr, grants_seen_gen)); F("S_CRC", offsetof(struct procctl_state_hdr, crc32c));
  F("E_ID", offsetof(struct procctl_grant_entry, resource_id)); F("E_GRANT", offsetof(struct procctl_grant_entry, grant_id)); F("E_DUR", offsetof(struct procctl_grant_entry, duration_ns));
  F("E_ACTIVITY", offsetof(struct procctl_res_entry, last_activity_ns)); F("E_KIND", offsetof(struct procctl_grant_entry, kind)); F("E_STATE", offsetof(struct procctl_res_entry, state));
  F("E_FLAGS", offsetof(struct procctl_res_entry, flags)); F("E_ERROR", offsetof(struct procctl_res_entry, error));
  F("RES_IDLE", PROCCTL_RES_IDLE); F("RES_ACQUIRING", PROCCTL_RES_ACQUIRING);
  F("RES_HOLDING", PROCCTL_RES_HOLDING); F("RES_RELEASED", PROCCTL_RES_RELEASED);
  F("RES_FAILED", PROCCTL_RES_FAILED); F("RES_F_WANT", PROCCTL_RES_F_WANT);
  printf("}\n");
  return 0;
}
