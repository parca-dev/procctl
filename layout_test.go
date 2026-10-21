//go:build linux

package procctl

import (
	"errors"
	"testing"
)

func validHeader() Header {
	return Header{
		Version: Version, PageSize: 4096, TotalSize: 8192, Consumer: "echo", SchemaVersion: 3,
		StateAddr: 0x1000, StateSize: 128, InstanceID: 99,
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
	if got.PayloadCapacity() != 4096-CtrlSize {
		t.Fatalf("payload capacity %d", got.PayloadCapacity())
	}
}

func TestParseHeaderRejects(t *testing.T) {
	cases := map[string]func(h *Header, b []byte) []byte{
		"magic":           func(h *Header, b []byte) []byte { b[0] = 'X'; return b },
		"version":         func(h *Header, b []byte) []byte { h.Version = 2; return nil },
		"size mismatch":   func(h *Header, b []byte) []byte { h.TotalSize = 4096 * 3; return nil },
		"page size":       func(h *Header, b []byte) []byte { h.PageSize = 1000; return nil },
		"too small":       func(h *Header, b []byte) []byte { h.TotalSize = 4096; return nil },
		"state unaligned": func(h *Header, b []byte) []byte { h.StateAddr = 0x1001; return nil },
		"no state block":  func(h *Header, b []byte) []byte { h.StateAddr = 0; return nil },
		"state too small": func(h *Header, b []byte) []byte { h.StateSize = 32; return nil },
		"state too large": func(h *Header, b []byte) []byte { h.StateSize = 70000; return nil },
		"consumer chars":  func(h *Header, b []byte) []byte { h.Consumer = "Echo"; return nil },
		"empty consumer":  func(h *Header, b []byte) []byte { h.Consumer = ""; return nil },
		"zero instance":   func(h *Header, b []byte) []byte { h.InstanceID = 0; return nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := validHeader()
			b := encodeHeader(h)
			if nb := mutate(&h, b); nb == nil {
				b = encodeHeader(h)
			}
			size := uint64(8192)
			if name == "too small" {
				size = 4096
			}
			if _, err := ParseHeader(b, size); !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestParseMemfdName(t *testing.T) {
	for link, want := range map[string]string{
		"/memfd:procctl:echo-v1":             "echo/1",
		"/memfd:procctl:gpu_v2-v1 (deleted)": "gpu_v2/1",
		"/memfd:other":                       "",
		"/memfd:procctl:echo":                "",
	} {
		c, v, ok := parseMemfdName(link)
		got := ""
		if ok {
			got = c + "/" + string(rune('0'+v))
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", link, got, want)
		}
	}
}
