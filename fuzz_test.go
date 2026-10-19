//go:build linux

package procctl

import (
	"bytes"
	"testing"
	"unsafe"
)

// Untrusted-input parsers must never panic and must only accept what they
// validated (SPEC §13).

func FuzzParseHeader(f *testing.F) {
	f.Add(encodeHeader(validHeader()), uint64(8192))
	f.Add(make([]byte, HeaderSize), uint64(0))
	f.Fuzz(func(t *testing.T, b []byte, size uint64) {
		h, err := ParseHeader(b, size)
		if err != nil {
			return
		}
		if h.TotalSize != size || h.AreaOff+h.AreaSize != h.TotalSize || h.ResourceSlots > MaxSlots {
			t.Fatalf("accepted inconsistent header: %+v", h)
		}
		if h.PayloadCapacity() < 0 {
			t.Fatalf("negative payload capacity: %+v", h)
		}
	})
}

func validStateBlock(h *Header, pid uint32) []byte {
	sh := stateHeader{Seq: 6, Instance_id: h.InstanceID, Writer_pid: pid, State_len: 5, Applied_gen: 4}
	copy(sh.Magic[:], stateMagic)
	table := make([]resEntry, h.ResourceSlots)
	table[0] = resEntry{Resource_id: [16]byte{0x42}, Grant_id: 4, Kind: 1, State: ResHolding}
	var buf bytes.Buffer
	encode(&buf, &sh)
	encode(&buf, table)
	buf.WriteString("hello")
	used := buf.Len()
	b := make([]byte, h.StateSize)
	copy(b, buf.Bytes())
	chk := append([]byte(nil), b[:used]...)
	clear(chk[stateSeqOff : stateSeqOff+8])
	le.PutUint32(b[stateCRCOff:], crc32c(chk))
	return b
}

func TestParseStateValid(t *testing.T) {
	h := validHeader()
	st, err := parseState(validStateBlock(&h, 77), &h, 77)
	if err != nil {
		t.Fatal(err)
	}
	if st.Stale || st.AppliedGen != 4 || string(st.Consumer) != "hello" || len(st.Resources) != 1 ||
		st.Resources[0].State != ResHolding || !st.Resources[0].MayHold() {
		t.Fatalf("%+v", st)
	}
	// Written by another pid (inherited across fork): stale, no holdings.
	st, err = parseState(validStateBlock(&h, 77), &h, 78)
	if err != nil || !st.Stale || len(st.Resources) != 0 {
		t.Fatalf("stale: %+v %v", st, err)
	}
	// Any flipped byte in the used range is rejected.
	for i := 0; i < StateHdrSize+2*EntrySize+5; i++ {
		if i >= stateSeqOff && i < stateSeqOff+8 {
			continue // seq is validated by the reader, not the CRC
		}
		b := validStateBlock(&h, 77)
		b[i] ^= 0x10
		if _, err := parseState(b, &h, 77); err == nil {
			t.Fatalf("flipped byte %d accepted", i)
		}
	}
}

func FuzzParseState(f *testing.F) {
	h := validHeader()
	f.Add(validStateBlock(&h, 77))
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, b []byte) {
		st, err := parseState(b, &h, 77)
		if err != nil {
			return
		}
		if len(st.Resources) > int(h.ResourceSlots) ||
			StateHdrSize+int(h.ResourceSlots)*EntrySize+len(st.Consumer) > len(b) {
			t.Fatalf("accepted out-of-bounds state: %+v", st)
		}
	})
}

// FuzzAreaRead: arbitrary bytes in the agent area (a hostile or buggy peer
// can't write it, but a reader must survive anything anyway).
func FuzzAreaRead(f *testing.F) {
	f.Add(make([]byte, 4096), uint8(2))
	f.Fuzz(func(t *testing.T, raw []byte, slots uint8) {
		mem := make([]uint64, 4096/8) // 8-aligned backing store
		body := unsafe.Slice((*byte)(unsafe.Pointer(&mem[0])), 4096)
		copy(body, raw)
		a := area{mem: body, body: body, slots: int(slots % (MaxSlots + 1))}
		gen, payload, grants, err := a.readPayload()
		if err != nil || gen == 0 {
			return
		}
		if len(grants) > a.slots || CtrlSize+a.slots*EntrySize+len(payload) > len(body) {
			t.Fatalf("accepted out-of-bounds payload")
		}
	})
}
