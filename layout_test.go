//go:build linux

package procctl

import (
	"errors"
	"testing"
)

func validHeader() Header {
	return Header{
		SpecMajor: 1, TotalSize: 8192, PageSize: 4096, Consumer: "echo", SchemaVersion: 3,
		CreatorPID: 7, AreaOff: 4096, AreaSize: 4096, StateAddr: 0x1000, StateSize: 64 + 2*48 + 64,
		ResourceSlots: 2, CreatedAtNS: 1, InstanceID: 99,
	}
}

func TestParseHeaderRoundTrip(t *testing.T) {
	h := validHeader()
	got, err := ParseHeader(encodeHeader(h), h.TotalSize)
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, h)
	}
	if got.PayloadCapacity() != 4096-64-2*48 {
		t.Fatalf("payload capacity %d", got.PayloadCapacity())
	}
}

func TestParseHeaderRejects(t *testing.T) {
	cases := map[string]func(h *Header, b []byte) []byte{
		"bad signature":      func(h *Header, b []byte) []byte { b[0] = 'X'; return b },
		"bad crc":            func(h *Header, b []byte) []byte { b[hSchema] ^= 1; return b },
		"major":              func(h *Header, b []byte) []byte { h.SpecMajor = 2; return nil },
		"size mismatch":      func(h *Header, b []byte) []byte { h.TotalSize = 4096 * 3; return nil },
		"page size":          func(h *Header, b []byte) []byte { h.PageSize = 1000; return nil },
		"area offset":        func(h *Header, b []byte) []byte { h.AreaOff = 0; return nil },
		"too many slots":     func(h *Header, b []byte) []byte { h.ResourceSlots = 65; return nil },
		"resources overflow": func(h *Header, b []byte) []byte { h.ResourceSlots = 4; h.StateSize = 64 + 3*48; return nil },
		"state unaligned":    func(h *Header, b []byte) []byte { h.StateAddr = 0x1001; return nil },
		"state too small":    func(h *Header, b []byte) []byte { h.StateSize = 64; return nil },
		"state too large":    func(h *Header, b []byte) []byte { h.StateSize = 70000; return nil },
		"size w/o addr":      func(h *Header, b []byte) []byte { h.StateAddr = 0; return nil },
		"consumer chars":     func(h *Header, b []byte) []byte { h.Consumer = "Echo"; return nil },
		"empty consumer":     func(h *Header, b []byte) []byte { h.Consumer = ""; return nil },
		"zero instance":      func(h *Header, b []byte) []byte { h.InstanceID = 0; return nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := validHeader()
			b := encodeHeader(h)
			if nb := mutate(&h, b); nb == nil {
				b = encodeHeader(h)
			}
			if _, err := ParseHeader(b, 8192); !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestParseMemfdName(t *testing.T) {
	for link, want := range map[string]struct {
		c  string
		m  int
		ok bool
	}{
		"/memfd:procctl:echo-v1 (deleted)": {"echo", 1, true},
		"/memfd:procctl:parca_gpu-v12":     {"parca_gpu", 12, true},
		"/memfd:procctl:echo":              {"", 0, false},
		"/memfd:OTEL_CTX (deleted)":        {"", 0, false},
		"/memfd:procctl:-v1":               {"", 0, false},
	} {
		c, m, ok := parseMemfdName(link)
		if c != want.c || m != want.m || ok != want.ok {
			t.Errorf("%q: got (%q,%d,%v) want %+v", link, c, m, ok, want)
		}
	}
}
