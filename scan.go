//go:build linux

package procctl

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// Candidate is a procctl memfd found in a process's fd table.
type Candidate struct {
	HostPID  int
	FD       int
	Consumer string
	Major    int
	Key      Key // from stat(2) through the /proc fd link
}

// ScanOptions configures Scan.
type ScanOptions struct {
	ProcRoot      string // default "/proc"
	Consumer      string // only this consumer; "" = all
	MaxFDsPerProc int    // default 65536
	PIDs          []int  // only these processes; nil = all
}

// ScanStats reports the cost of a scan.
type ScanStats struct {
	Procs, FDs int
	Duration   time.Duration
}

// Scan walks /proc/*/fd for procctl memfds (SPEC §10.1).
func Scan(ctx context.Context, opt ScanOptions) ([]Candidate, ScanStats, error) {
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.MaxFDsPerProc == 0 {
		opt.MaxFDsPerProc = 65536
	}
	start := time.Now()
	var stats ScanStats
	pids := opt.PIDs
	if pids == nil {
		ents, err := os.ReadDir(opt.ProcRoot)
		if err != nil {
			return nil, stats, err
		}
		for _, e := range ents {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	var out []Candidate
	for _, pid := range pids {
		if ctx.Err() != nil {
			return out, stats, ctx.Err()
		}
		stats.Procs++
		dir := fmt.Sprintf("%s/%d/fd", opt.ProcRoot, pid)
		f, err := os.Open(dir)
		if err != nil {
			continue // exited, or not permitted
		}
		names, _ := f.Readdirnames(opt.MaxFDsPerProc)
		f.Close()
		for _, n := range names {
			stats.FDs++
			link, err := os.Readlink(dir + "/" + n)
			if err != nil || len(link) < len(memfdPrefix) || link[:len(memfdPrefix)] != memfdPrefix {
				continue
			}
			consumer, major, ok := parseMemfdName(link)
			if !ok || (opt.Consumer != "" && consumer != opt.Consumer) {
				continue
			}
			fd, _ := strconv.Atoi(n)
			c := Candidate{HostPID: pid, FD: fd, Consumer: consumer, Major: major}
			var st syscall.Stat_t
			if syscall.Stat(dir+"/"+n, &st) == nil {
				c.Key = Key{Dev: uint64(st.Dev), Ino: st.Ino}
			}
			out = append(out, c)
		}
	}
	stats.Duration = time.Since(start)
	return out, stats, nil
}
