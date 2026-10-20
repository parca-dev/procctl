//go:build linux

// Package hello gives a procctl agent immediate discovery (plan §2.6):
//
//   - It finds the procctl:hello USDT probe (SPEC §11.1) in the ELF notes of
//     executable files that processes have mapped, and attaches a uprobe to
//     each call site. A uprobe is per file (inode), so it covers every process
//     that uses that binary or library, including ones started later.
//   - When the probe fires (a process created a region, or re-created one
//     after fork), it reports the pid.
//   - Executable mappings come from a startup sweep of /proc/*/maps and then
//     from perf mmap2 sideband events, so new binaries and libraries get their
//     uprobe as soon as they are mapped. After attaching to a newly seen file
//     it also reports that pid once, which closes the race with a region
//     created just before the uprobe existed.
//
// Everything reported is a hint: the agent still discovers and validates
// regions through /proc (SPEC §10).
//
// Needs CAP_BPF and CAP_PERFMON (the program, mapping events), CAP_SYS_ADMIN
// for the uprobe PMU, and ptrace-read access to processes (/proc/<pid>/root).
// Without CAP_SYS_ADMIN it degrades to mapping events (Stats.UprobeErrors);
// if New fails the agent should run scan-only. Pids are reported in the
// agent's pid namespace (see pidns.go).
package hello

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/parca-dev/usdt"
	"golang.org/x/sys/unix"

	"github.com/parca-dev/procctl/internal/perfsideband"
)

// Options configures a Watcher.
type Options struct {
	// Notify is called with the pid of a process that (probably) just created
	// a procctl region. Typically procctl.Registry.Notify.
	Notify func(pid int)
	// ProcRoot is the procfs mount; default "/proc".
	ProcRoot string
	// NoMappingEvents disables perf mmap2 events; new files are then only
	// found by Resweep.
	NoMappingEvents bool
	// GCInterval is how often uprobes of files that no process maps any more
	// are detached (one /proc/*/maps pass); default 2 minutes. With overlayfs
	// every container's copy of a binary is a file of its own.
	GCInterval time.Duration
	Logger     *slog.Logger
}

// Stats counts what the watcher has done.
type Stats struct {
	FilesInspected  uint64 // distinct executable files whose ELF notes were read
	FilesWithHello  uint64 // ... that contain procctl:hello
	Uprobes         uint64 // uprobes attached
	UprobesActive   uint64 // ... and not yet detached by GC
	FilesCached     uint64 // files in the inspection cache
	UprobeErrors    uint64 // uprobes that couldn't be attached
	Firings         uint64 // hello probe hits
	MappingEvents   uint64 // mmap2 records seen
	LostEvents      uint64
	InspectFailures uint64
}

type fileKey struct {
	dev uint64
	ino uint64
}

type fileState struct {
	hasHello bool
	links    []link.Link
	lastSeen time.Time // last mapping event or sweep that saw it mapped
}

// Watcher watches for procctl:hello.
type Watcher struct {
	opt   Options
	log   *slog.Logger
	prog  *ebpf.Program
	rb    *ebpf.Map
	rd    *ringbuf.Reader
	side  *perfsideband.Reader
	mu    sync.Mutex
	files map[fileKey]*fileState
	stats struct {
		inspected, withHello, uprobes, uprobeErrs, detached, firings, events, lost, fails atomic.Uint64
	}
	warnOnce sync.Once
	resweep  chan struct{}
}

// New loads the uprobe program. It fails without the needed privileges.
func New(opt Options) (*Watcher, error) {
	if opt.Notify == nil {
		return nil, errors.New("hello: Notify is required")
	}
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.GCInterval == 0 {
		opt.GCInterval = 2 * time.Minute
	}
	_ = rlimit.RemoveMemlock() // kernels < 5.11 account BPF memory to RLIMIT_MEMLOCK
	rb, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.RingBuf, MaxEntries: 64 << 10})
	if err != nil {
		return nil, fmt.Errorf("hello: ring buffer: %w", err)
	}
	ns, err := selfPidns(opt.ProcRoot)
	if err != nil {
		rb.Close()
		return nil, fmt.Errorf("hello: pid namespace: %w", err)
	}
	var offs *taskOffsets
	if !ns.root {
		o, err := kernelTaskOffsets()
		if err != nil {
			rb.Close()
			return nil, fmt.Errorf("hello: agent not in the root pid namespace and can't translate pids: %w", err)
		}
		offs = &o
		opt.Logger.Info("hello: translating pids to the agent's pid namespace", "pidns", ns.inum)
	}
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         "procctl_hello",
		Type:         ebpf.Kprobe, // uprobes use the kprobe program type
		License:      "GPL",       // bpf_probe_read_kernel is GPL-only
		Instructions: helloProgram(rb, ns, offs),
	})
	if err != nil {
		rb.Close()
		return nil, fmt.Errorf("hello: load program: %w", err)
	}
	rd, err := ringbuf.NewReader(rb)
	if err != nil {
		prog.Close()
		rb.Close()
		return nil, err
	}
	return &Watcher{
		opt: opt, log: opt.Logger, prog: prog, rb: rb, rd: rd,
		files: map[fileKey]*fileState{}, resweep: make(chan struct{}, 1),
	}, nil
}

// Run sweeps existing processes, starts mapping events, and reports probe
// firings until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	go w.readFirings()
	if !w.opt.NoMappingEvents {
		side, err := perfsideband.Start(ctx, perfsideband.Config{
			OnMapping: w.onMapping,
			OnLost: func(n uint64) {
				w.stats.lost.Add(n)
				w.Resweep()
			},
			OnError: func(cpu int, err error) { w.log.Warn("mapping events", "cpu", cpu, "err", err) },
		})
		if err != nil {
			w.log.Warn("mapping events unavailable; new files are found by resweeps only", "err", err)
		} else {
			w.side = side
		}
	}
	w.sweep(true)
	gc := time.NewTicker(w.opt.GCInterval)
	defer gc.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.resweep:
			w.sweep(true)
		case <-gc.C:
			w.gc()
		}
	}
}

// Resweep schedules another pass over every process's executable mappings
// (e.g. after lost mapping events).
func (w *Watcher) Resweep() {
	select {
	case w.resweep <- struct{}{}:
	default:
	}
}

func (w *Watcher) readFirings() {
	// One hello call fires every uprobe on that file: with overlayfs each
	// container's copy of a binary has its own, so coalesce repeats.
	var lastPid int
	var lastAt time.Time
	for {
		rec, err := w.rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			continue
		}
		if len(rec.RawSample) >= 4 {
			pid := int(uint32(rec.RawSample[0]) | uint32(rec.RawSample[1])<<8 |
				uint32(rec.RawSample[2])<<16 | uint32(rec.RawSample[3])<<24)
			w.stats.firings.Add(1)
			if pid == lastPid && time.Since(lastAt) < 100*time.Millisecond {
				continue
			}
			lastPid, lastAt = pid, time.Now()
			w.log.Debug("hello fired", "pid", pid)
			w.opt.Notify(pid)
		}
	}
}

func (w *Watcher) onMapping(m perfsideband.Mapping) {
	w.stats.events.Add(1)
	key := fileKey{dev: unix.Mkdev(m.Major, m.Minor), ino: m.Inode}
	if w.inspect(m.PID, key, m.Filename) {
		w.opt.Notify(m.PID)
	}
}

// gc detaches the uprobes of files that no process maps any more and drops
// them from the cache: a sweep refreshes lastSeen of every mapped file, so
// whatever wasn't seen since the sweep began is gone. (A file mapped again
// later is re-inspected on its mapping event.)
func (w *Watcher) gc() {
	start := time.Now()
	w.sweep(false)
	var stale []link.Link
	dropped := 0
	w.mu.Lock()
	for k, f := range w.files {
		if f.lastSeen.Before(start) {
			stale = append(stale, f.links...)
			delete(w.files, k)
			dropped++
		}
	}
	w.mu.Unlock()
	for _, l := range stale {
		l.Close()
	}
	w.stats.detached.Add(uint64(len(stale)))
	w.log.Debug("hello gc", "files_dropped", dropped, "uprobes_detached", len(stale), "took", time.Since(start))
}

// sweep inspects every executable file mapping of every process; with notify
// it reports the processes that map a file with hello sites.
func (w *Watcher) sweep(notify bool) {
	ents, err := os.ReadDir(w.opt.ProcRoot)
	if err != nil {
		w.log.Warn("sweep", "err", err)
		return
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		hasHello := false
		for _, m := range w.execMappings(pid) {
			w.inspect(pid, m.key, m.path)
			w.mu.Lock()
			if f := w.files[m.key]; f != nil && f.hasHello {
				hasHello = true
			}
			w.mu.Unlock()
		}
		if hasHello && notify {
			w.opt.Notify(pid) // may already have a region: let the registry look
		}
	}
}

type execMapping struct {
	key  fileKey
	path string
}

// execMappings parses /proc/<pid>/maps for executable file mappings.
func (w *Watcher) execMappings(pid int) []execMapping {
	f, err := os.Open(fmt.Sprintf("%s/%d/maps", w.opt.ProcRoot, pid))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []execMapping
	seen := map[fileKey]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// addr perms offset dev inode path
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || len(fields[1]) < 3 || fields[1][2] != 'x' || fields[4] == "0" {
			continue
		}
		maj, min, ok := strings.Cut(fields[3], ":")
		if !ok {
			continue
		}
		ma, err1 := strconv.ParseUint(maj, 16, 32)
		mi, err2 := strconv.ParseUint(min, 16, 32)
		ino, err3 := strconv.ParseUint(fields[4], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		key := fileKey{dev: unix.Mkdev(uint32(ma), uint32(mi)), ino: ino}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, execMapping{key: key, path: strings.Join(fields[5:], " ")})
	}
	return out
}

// inspect reads the file's USDT notes once (cached by inode) and attaches
// uprobes to procctl:hello. It reports whether the caller should look at the
// pid once: this call attached new uprobes, or the file has hello sites but
// no uprobes (degraded: the mapping event is then the only signal).
func (w *Watcher) inspect(pid int, key fileKey, path string) bool {
	w.mu.Lock()
	if st, done := w.files[key]; done {
		st.lastSeen = time.Now()
		look := st.hasHello && len(st.links) == 0
		w.mu.Unlock()
		return look
	}
	st := &fileState{lastSeen: time.Now()}
	w.files[key] = st // even on failure: don't retry on every mapping
	w.mu.Unlock()

	// The path is in the process's mount namespace: reach it through
	// /proc/<pid>/root.
	full := fmt.Sprintf("%s/%d/root%s", w.opt.ProcRoot, pid, path)
	f, err := os.Open(full)
	if err != nil {
		w.stats.fails.Add(1)
		return false
	}
	defer f.Close()
	var fst unix.Stat_t
	if unix.Fstat(int(f.Fd()), &fst) != nil || fst.Dev != key.dev || fst.Ino != key.ino {
		w.stats.fails.Add(1) // replaced since it was mapped
		return false
	}
	w.stats.inspected.Add(1)
	locs, err := helloLocations(f)
	if err != nil || len(locs) == 0 {
		return false
	}
	w.stats.withHello.Add(1)
	ex, err := link.OpenExecutable(full)
	if err != nil {
		w.log.Warn("open executable", "path", full, "err", err)
		return false
	}
	var links []link.Link
	for _, loc := range locs {
		l, err := ex.Uprobe("", w.prog, &link.UprobeOptions{Address: loc})
		if err != nil {
			w.stats.uprobeErrs.Add(1)
			if errors.Is(err, os.ErrPermission) {
				// The uprobe PMU wants CAP_SYS_ADMIN (CAP_PERFMON isn't
				// enough, at least through 7.0): the same for every file.
				w.warnOnce.Do(func() {
					w.log.Warn("hello uprobes unavailable (needs CAP_SYS_ADMIN); discovery uses mapping events and rescans", "err", err)
				})
			} else {
				w.log.Warn("attach hello uprobe", "path", path, "offset", loc, "err", err)
			}
			continue
		}
		links = append(links, l)
		w.stats.uprobes.Add(1)
	}
	w.mu.Lock()
	if w.files[key] != st { // dropped by gc meanwhile: the process is gone
		w.mu.Unlock()
		for _, l := range links {
			l.Close()
		}
		w.stats.detached.Add(uint64(len(links)))
		return false
	}
	st.hasHello, st.links = true, links
	w.mu.Unlock()
	if len(links) > 0 {
		w.log.Info("hello uprobes attached", "path", path, "pid", pid, "sites", len(st.links))
	}
	return st.hasHello
}

// helloLocations returns the file offsets of procctl:hello call sites.
func helloLocations(r io.ReaderAt) ([]uint64, error) {
	er, err := usdt.NewStdlibELFReader(r)
	if err != nil {
		return nil, err // not ELF
	}
	probes, err := usdt.ParseProbes(er)
	if err != nil {
		return nil, err
	}
	var locs []uint64
	for _, p := range probes {
		if p.Provider == "procctl" && p.Name == "hello" {
			locs = append(locs, p.Location)
		}
	}
	return locs, nil
}

// Stats returns counters.
func (w *Watcher) Stats() Stats {
	w.mu.Lock()
	cached := uint64(len(w.files))
	w.mu.Unlock()
	up := w.stats.uprobes.Load()
	return Stats{
		UprobesActive: up - w.stats.detached.Load(), FilesCached: cached,
		FilesInspected: w.stats.inspected.Load(), FilesWithHello: w.stats.withHello.Load(),
		Uprobes: w.stats.uprobes.Load(), UprobeErrors: w.stats.uprobeErrs.Load(), Firings: w.stats.firings.Load(),
		MappingEvents: w.stats.events.Load(), LostEvents: w.stats.lost.Load(),
		InspectFailures: w.stats.fails.Load(),
	}
}

// Close detaches every uprobe and releases the program and events.
func (w *Watcher) Close() error {
	if w.side != nil {
		w.side.Close()
	}
	w.mu.Lock()
	for _, f := range w.files {
		for _, l := range f.links {
			l.Close()
		}
	}
	w.files = map[fileKey]*fileState{}
	w.mu.Unlock()
	w.rd.Close()
	w.prog.Close()
	return w.rb.Close()
}
