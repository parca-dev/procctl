//go:build linux

package procctl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"

	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// ---- layout: C macros vs Go constants -------------------------------------------

func TestLayoutMatchesC(t *testing.T) {
	bin := buildC(t, "c/tests/layout_dump.c")
	out, err := exec.Command(bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]int
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var wh wireHeader
	var cb ctrlBlock
	var g Grant
	var sh stateHeader
	var re ResourceEntry
	off := func(o uintptr) int { return int(o) }
	want := map[string]int{
		"HEADER_SIZE": HeaderSize, "CTRL_SIZE": CtrlSize, "ENTRY_SIZE": EntrySize,
		"STATE_HDR_SIZE": StateHdrSize, "MAX_SLOTS": MaxSlots, "MAX_STATE_SIZE": MaxStateSize,
		"SPEC_MAJOR": SpecMajor, "SPEC_MINOR": SpecMinor,
		"H_SIG": off(unsafe.Offsetof(wh.Signature)), "H_MAJOR": off(unsafe.Offsetof(wh.SpecMajor)),
		"H_MINOR": off(unsafe.Offsetof(wh.SpecMinor)), "H_HSIZE": off(unsafe.Offsetof(wh.HeaderSize)),
		"H_TOTAL": off(unsafe.Offsetof(wh.TotalSize)), "H_PAGE": off(unsafe.Offsetof(wh.PageSize)),
		"H_CONSUMER": off(unsafe.Offsetof(wh.Consumer)), "H_SCHEMA": off(unsafe.Offsetof(wh.SchemaVersion)),
		"H_CREATOR": off(unsafe.Offsetof(wh.CreatorPID)), "H_AREA_OFF": off(unsafe.Offsetof(wh.AgentAreaOff)),
		"H_AREA_SIZE": off(unsafe.Offsetof(wh.AgentAreaSize)), "H_STATE_ADDR": off(unsafe.Offsetof(wh.StateAddr)),
		"H_STATE_SIZE": off(unsafe.Offsetof(wh.StateSize)), "H_SLOTS": off(unsafe.Offsetof(wh.ResourceSlots)),
		"H_CREATED": off(unsafe.Offsetof(wh.CreatedAtNS)), "H_INSTANCE": off(unsafe.Offsetof(wh.InstanceID)),
		"H_CRC": off(unsafe.Offsetof(wh.HeaderCRC)),
		"C_SEQ": off(unsafe.Offsetof(cb.Seq)), "C_LEN": off(unsafe.Offsetof(cb.PayloadLen)),
		"C_FLAGS": off(unsafe.Offsetof(cb.Flags)), "C_WRITER": off(unsafe.Offsetof(cb.WriterID)),
		"C_HB": off(unsafe.Offsetof(cb.HeartbeatNS)), "C_LEASE": off(unsafe.Offsetof(cb.LeaseNS)),
		"C_CRC":   off(unsafe.Offsetof(cb.PayloadCRC32)),
		"S_MAGIC": off(unsafe.Offsetof(sh.Magic)), "S_SEQ": off(unsafe.Offsetof(sh.Seq)),
		"S_INSTANCE": off(unsafe.Offsetof(sh.InstanceID)), "S_PID": off(unsafe.Offsetof(sh.WriterPID)),
		"S_LEN": off(unsafe.Offsetof(sh.StateLen)), "S_APPLIED": off(unsafe.Offsetof(sh.AppliedGen)),
		"S_APPLIED_W": off(unsafe.Offsetof(sh.AppliedWriterID)), "S_SEEN": off(unsafe.Offsetof(sh.GrantsSeenGen)),
		"S_CRC": off(unsafe.Offsetof(sh.CRC32)),
		"E_ID":  off(unsafe.Offsetof(g.ID)), "E_GRANT": off(unsafe.Offsetof(g.GrantID)),
		"E_DUR": off(unsafe.Offsetof(g.Duration)), "E_KIND": off(unsafe.Offsetof(g.Kind)),
		"E_ACTIVITY": off(unsafe.Offsetof(re.LastActivityNS)), "E_STATE": off(unsafe.Offsetof(re.State)),
		"E_FLAGS": off(unsafe.Offsetof(re.Flags)), "E_ERROR": off(unsafe.Offsetof(re.Error)),
		"RES_IDLE": ResIdle, "RES_ACQUIRING": ResAcquiring, "RES_HOLDING": ResHolding,
		"RES_RELEASED": ResReleased, "RES_FAILED": ResFailed, "RES_F_WANT": ResFlagWant,
	}
	// The grant and resource entries share id/grant_id/kind positions.
	if unsafe.Offsetof(g.ID) != unsafe.Offsetof(re.ID) || unsafe.Offsetof(g.GrantID) != unsafe.Offsetof(re.GrantID) ||
		unsafe.Offsetof(g.Kind) != unsafe.Offsetof(re.Kind) {
		t.Error("grant and resource entries disagree on shared fields")
	}
	for k, v := range want {
		if got, ok := c[k]; !ok || got != v {
			t.Errorf("%s: C=%d (present=%v) Go=%d", k, got, ok, v)
		}
	}
	for k := range c {
		if _, ok := want[k]; !ok {
			t.Errorf("C exports %s, which Go does not check", k)
		}
	}
}

// ---- stress: Go writer ↔ C readers, C state writer ↔ Go reader ------------------

type stressStats struct {
	Polls      uint64 `json:"polls"`
	Changes    uint64 `json:"changes"`
	Busy       uint64 `json:"busy"`
	Bad        uint64 `json:"bad"`
	Backwards  uint64 `json:"backwards"`
	GrantReads uint64 `json:"grant_reads"`
	GrantBad   uint64 `json:"grant_bad"`
	GrantBusy  uint64 `json:"grant_busy"`
	GrantOther int    `json:"grant_other"`
}

func stressPattern(tag uint64, i int) byte {
	x := tag*0x9e3779b97f4a7c15 + uint64(i)
	x ^= x >> 31
	x *= 0xbf58476d1ce4e5b9
	return byte(x >> 56)
}

func TestStress(t *testing.T) {
	secs := 3
	if s := os.Getenv("PROCCTL_STRESS_SECONDS"); s != "" {
		secs, _ = strconv.Atoi(s)
	}
	if testing.Short() {
		secs = 1
	}
	bin := buildC(t, "c/tests/stress_client.c")
	const n = 4
	type sp struct {
		cmd   *exec.Cmd
		stdin interface{ Close() error }
		stats chan stressStats
	}
	var procs []*sp
	for i := 0; i < n; i++ {
		cmd := exec.Command(bin)
		stdin, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		p := &sp{cmd: cmd, stdin: stdin, stats: make(chan stressStats, 1)}
		ready := make(chan struct{})
		go func() {
			s := bufio.NewScanner(out)
			for s.Scan() {
				var m map[string]any
				_ = json.Unmarshal(s.Bytes(), &m)
				switch m["ev"] {
				case "ready":
					close(ready)
				case "stats":
					var st stressStats
					b, _ := json.Marshal(m)
					_ = json.Unmarshal(b, &st)
					p.stats <- st
				}
			}
		}()
		<-ready
		procs = append(procs, p)
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	}

	id, _ := NewIdentity()
	var wg sync.WaitGroup
	var writes, stateReads, stateBusy atomic.Uint64
	stop := make(chan struct{})
	var mu sync.Mutex
	var failures []string
	fail := func(f string, a ...any) {
		mu.Lock()
		failures = append(failures, sprintf(f, a...))
		mu.Unlock()
	}
	for i, p := range procs {
		cands, _, _ := Scan(context.Background(), ScanOptions{Consumer: "stress"})
		var fd = -1
		for _, c := range cands {
			if c.HostPID == p.cmd.Process.Pid {
				fd = c.FD
			}
		}
		c, err := Attach(p.cmd.Process.Pid, fd, AttachOptions{Identity: id, Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		wg.Add(2)
		go func(seed int64) { // writer
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for tag := uint64(1); ; tag++ {
				select {
				case <-stop:
					return
				default:
				}
				size := 12 + rng.Intn(c.Info().PayloadCap-12)
				buf := make([]byte, size)
				le.PutUint64(buf, tag)
				le.PutUint32(buf[8:], uint32(size))
				for j := 12; j < size; j++ {
					buf[j] = stressPattern(tag, j)
				}
				var grants []Grant
				for s := 0; s < rng.Intn(5); s++ {
					var gid [16]byte
					for k := range gid {
						gid[k] = byte(s + 1)
					}
					grants = append(grants, Grant{Kind: 7, ID: gid, Duration: 1000 + uint64(s+1)})
				}
				if _, _, err := c.Write(buf, grants); err != nil {
					fail("write: %v", err)
					return
				}
				writes.Add(1)
				// Bursts with short gaps: thousands of writes per second, but a
				// continuously-writing writer would starve every reader (inherent
				// to seqlocks; real agents write rarely).
				if rng.Intn(4) != 0 {
					time.Sleep(time.Duration(rng.Intn(200)) * time.Microsecond)
				}
			}
		}(int64(i))
		go func() { // state reader
			defer wg.Done()
			last := uint64(0)
			for {
				select {
				case <-stop:
					return
				default:
				}
				st, err := c.ReadState(context.Background())
				if err != nil {
					stateBusy.Add(1)
					continue
				}
				stateReads.Add(1)
				b := st.Consumer
				if len(b) < 16 {
					continue
				}
				nn := le.Uint64(b)
				if int(le.Uint64(b[8:])) != len(b) {
					fail("state length mismatch")
				}
				for j := 16; j < len(b); j++ {
					if b[j] != stressPattern(nn, j) {
						fail("torn state got through: n=%d byte %d", nn, j)
						break
					}
				}
				if nn < last {
					fail("state went backwards: %d after %d", nn, last)
				}
				last = nn
			}
		}()
	}
	time.Sleep(time.Duration(secs) * time.Second)
	close(stop)
	wg.Wait()
	var total stressStats
	for _, p := range procs {
		p.stdin.Close()
		st := <-p.stats
		_ = p.cmd.Wait()
		total.Polls += st.Polls
		total.Changes += st.Changes
		total.Busy += st.Busy
		total.Bad += st.Bad
		total.Backwards += st.Backwards
		total.GrantReads += st.GrantReads
		total.GrantBad += st.GrantBad
		total.GrantBusy += st.GrantBusy
		if st.GrantOther != 0 {
			total.GrantOther = st.GrantOther
		}
	}
	t.Logf("%ds, %d processes: Go wrote %d generations; C polled %d times, applied %d, busy %d, grant reads %d; Go state reads %d (busy %d)",
		secs, n, writes.Load(), total.Polls, total.Changes, total.Busy, total.GrantReads, stateReads.Load(), stateBusy.Load())
	if total.Bad != 0 || total.Backwards != 0 || total.GrantBad != 0 {
		t.Errorf("C readers accepted torn data: %+v", total)
	}
	if len(failures) > 0 {
		t.Errorf("Go side: %d failures, first: %s", len(failures), failures[0])
	}
	if total.Changes < 1000 || stateReads.Load() < 1000 || total.GrantReads < 1000 {
		t.Errorf("stress did no work: %+v", total)
	}

}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
