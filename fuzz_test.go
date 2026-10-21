//go:build linux

package procctl

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The parsers read input from untrusted processes: they must never panic and
// must only accept what they validated.

func FuzzParseHeader(f *testing.F) {
	f.Add(encodeHeader(validHeader()), uint64(8192))
	f.Add(make([]byte, HeaderSize), uint64(0))
	f.Fuzz(func(t *testing.T, b []byte, size uint64) {
		h, err := ParseHeader(b, size)
		if err != nil {
			return
		}
		if h.TotalSize != size || h.PayloadCapacity() < 0 || h.StateSize > MaxStateSize {
			t.Fatalf("accepted inconsistent header: %+v", h)
		}
	})
}

func validStateBlock(h *Header, pid uint32, consumer string) []byte {
	sb := stateBlock{Seq: 6, Instance_id: h.InstanceID, Applied_gen: 4, Pid: pid, Len: uint32(len(consumer))}
	copy(sb.Magic[:], stateMagic)
	var buf bytes.Buffer
	_ = binary.Write(&buf, le, &sb)
	buf.WriteString(consumer)
	b := make([]byte, h.StateSize)
	copy(b, buf.Bytes())
	return b
}

func TestParseState(t *testing.T) {
	h := validHeader()
	st, err := parseState(validStateBlock(&h, 77, "hello"), &h, 77)
	if err != nil || st.Stale || st.AppliedGen != 4 || string(st.Consumer) != "hello" {
		t.Fatalf("%+v %v", st, err)
	}
	// Written by another pid (inherited across fork): stale, reports nothing.
	st, err = parseState(validStateBlock(&h, 77, "hello"), &h, 78)
	if err != nil || !st.Stale || st.AppliedGen != 0 || st.Consumer != nil {
		t.Fatalf("%+v %v", st, err)
	}
	bad := validStateBlock(&h, 77, "hello")
	bad[0] = 'X'
	if _, err := parseState(bad, &h, 77); err != ErrStateInvalid {
		t.Fatalf("bad magic: %v", err)
	}
	long := validStateBlock(&h, 77, "hello")
	le.PutUint32(long[StateSize-4:], uint32(h.StateSize))
	if _, err := parseState(long, &h, 77); err != ErrStateInvalid {
		t.Fatalf("overlong state: %v", err)
	}
}

func FuzzParseState(f *testing.F) {
	h := validHeader()
	f.Add(validStateBlock(&h, 77, "hello"))
	f.Fuzz(func(t *testing.T, b []byte) {
		st, err := parseState(b, &h, 77)
		if err == nil && len(st.Consumer) > len(b) {
			t.Fatal("consumer state longer than the block")
		}
	})
}

// The C reader and this writer agree: a write is read back intact, and a
// corrupted payload is never accepted.
func TestAreaWriteRead(t *testing.T) {
	a := area{body: make([]byte, 4096)}
	gen, err := a.write([]byte("hello"))
	if err != nil || gen != 2 {
		t.Fatal(gen, err)
	}
	g, p, err := a.read()
	if err != nil || g != gen || string(p) != "hello" {
		t.Fatal(g, string(p), err)
	}
	a.body[CtrlSize] ^= 0xff
	if _, _, err := a.read(); err == nil {
		t.Fatal("corrupted payload accepted")
	}
	if _, err := a.write(make([]byte, 4096)); err != ErrTooLarge {
		t.Fatal(err)
	}
}
