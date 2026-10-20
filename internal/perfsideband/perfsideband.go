//go:build linux

// Package perfsideband reads perf sideband records (here: PERF_RECORD_MMAP2
// for executable mappings) from one PERF_COUNT_SW_DUMMY event per online CPU.
//
// Adapted from go.opentelemetry.io/ebpf-profiler internal/perfutil
// (perfsidebandreader.go), Copyright The OpenTelemetry Authors, Apache-2.0.
package perfsideband

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/elastic/go-perf"
)

const maxConsecutiveFailedRecords = 100

// Mapping is one executable file-backed mapping reported by the kernel.
type Mapping struct {
	PID          int
	Major, Minor uint32
	Inode        uint64
	Filename     string // as seen in the process's mount namespace
}

// Config configures a Reader.
type Config struct {
	RingPages int               // per-CPU ring size in pages; default 8
	OnMapping func(Mapping)     // called for every executable file mapping
	OnLost    func(lost uint64) // records were dropped: callers should resync
	OnError   func(cpu int, err error)
}

// Reader owns one dummy perf event per CPU and forwards mmap2 records.
type Reader struct {
	events []*perf.Event
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start opens the events on every online CPU and starts reading. It needs
// CAP_PERFMON (or CAP_SYS_ADMIN, or perf_event_paranoid <= 0).
func Start(ctx context.Context, cfg Config) (*Reader, error) {
	if cfg.OnMapping == nil {
		return nil, errors.New("perfsideband: OnMapping is required")
	}
	if cfg.RingPages == 0 {
		cfg.RingPages = 8
	}
	cpus, err := onlineCPUs()
	if err != nil {
		return nil, err
	}
	attr := new(perf.Attr)
	perf.Dummy.Configure(attr)
	// Keep the events disabled until every ring is mapped.
	attr.Options.Disabled = true
	// Wake on any data: sideband records don't count as samples.
	attr.SetWakeupWatermark(1)
	// The kernel only emits MMAP2 records when Mmap is also set. Without
	// MmapData, only executable mappings are reported.
	attr.Options.Mmap = true
	attr.Options.Mmap2 = true

	r := &Reader{}
	for _, cpu := range cpus {
		ev, err := perf.Open(attr, perf.AllThreads, cpu, nil)
		if err != nil {
			r.closeEvents()
			return nil, fmt.Errorf("perf_event_open on CPU %d: %w", cpu, err)
		}
		r.events = append(r.events, ev)
		if err := ev.MapRingNumPages(cfg.RingPages); err != nil {
			r.closeEvents()
			return nil, fmt.Errorf("mapping ring on CPU %d: %w", cpu, err)
		}
	}
	for _, ev := range r.events {
		if err := ev.Enable(); err != nil {
			r.closeEvents()
			return nil, err
		}
	}
	ctx, r.cancel = context.WithCancel(ctx)
	for i, ev := range r.events {
		cpu := cpus[i]
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			if err := r.read(ctx, ev, cfg); err != nil && cfg.OnError != nil {
				cfg.OnError(cpu, err)
			}
		}()
	}
	return r, nil
}

func (r *Reader) read(ctx context.Context, ev *perf.Event, cfg Config) error {
	failed := 0
	for ctx.Err() == nil {
		rec, err := ev.ReadRecord(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if terminal(err) {
				return err
			}
			if failed++; failed >= maxConsecutiveFailedRecords {
				return err
			}
			continue
		}
		failed = 0
		switch rec := rec.(type) {
		case *perf.Mmap2Record:
			if rec.Inode != 0 { // anonymous executable memory (JITs) has no file
				cfg.OnMapping(Mapping{
					PID: int(rec.Pid), Major: rec.MajorID, Minor: rec.MinorID,
					Inode: rec.Inode, Filename: rec.Filename,
				})
			}
		case *perf.LostRecord:
			if cfg.OnLost != nil {
				cfg.OnLost(rec.Lost)
			}
		}
	}
	return nil
}

func terminal(err error) bool {
	if errors.Is(err, perf.ErrNoReadRecord) || errors.Is(err, perf.ErrDisabled) ||
		errors.Is(err, os.ErrClosed) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var se *os.SyscallError
	return errors.As(err, &se)
}

// Close stops the readers, then disables and closes the events (in that
// order: go-perf may otherwise send on a closed channel).
func (r *Reader) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	r.closeEvents()
}

func (r *Reader) closeEvents() {
	for _, ev := range r.events {
		_ = ev.Disable()
		_ = ev.Close()
	}
	r.events = nil
}

// onlineCPUs parses /sys/devices/system/cpu/online ("0-3,5,7-8").
func onlineCPUs() ([]int, error) {
	b, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, err
	}
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(string(b)), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, err
			}
		}
		for c := a; c <= b; c++ {
			cpus = append(cpus, c)
		}
	}
	return cpus, nil
}
