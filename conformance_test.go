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
)

// ---- layout: types_gen.go must match c/procctl.h ----------------------------------

// The wire structs exist only in c/procctl.h; types_gen.go is generated from
// it with `go tool cgo -godefs`. Fail if someone changed one without the other.
func TestGeneratedTypesUpToDate(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler")
	}
	out, err := exec.Command("scripts/gen-types.sh", "--check").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// ---- stress: Go writer ↔ C readers, C state writer ↔ Go reader ------------------

type stressStats struct {
	Polls     uint64 `json:"polls"`
	Changes   uint64 `json:"changes"`
	Busy      uint64 `json:"busy"`
	Bad       uint64 `json:"bad"`
	Backwards uint64 `json:"backwards"`
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

	id := NewOwnerID()
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
		c, err := Attach(p.cmd.Process.Pid, fd, AttachOptions{OwnerID: id, Lease: time.Minute})
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
				size := 12 + rng.Intn(c.Info().Header.PayloadCapacity()-12)
				buf := make([]byte, size)
				le.PutUint64(buf, tag)
				le.PutUint32(buf[8:], uint32(size))
				for j := 12; j < size; j++ {
					buf[j] = stressPattern(tag, j)
				}
				if _, err := c.Write(buf); err != nil {
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
	}
	t.Logf("%ds, %d processes: Go wrote %d generations; C polled %d times, applied %d, busy %d; Go state reads %d (busy %d)",
		secs, n, writes.Load(), total.Polls, total.Changes, total.Busy, stateReads.Load(), stateBusy.Load())
	if total.Bad != 0 || total.Backwards != 0 {
		t.Errorf("C readers accepted torn data: %+v", total)
	}
	if len(failures) > 0 {
		t.Errorf("Go side: %d failures, first: %s", len(failures), failures[0])
	}
	if total.Changes < 1000 || stateReads.Load() < 1000 {
		t.Errorf("stress did no work: %+v", total)
	}

}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
