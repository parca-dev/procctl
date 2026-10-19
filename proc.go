//go:build linux

package procctl

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// nsPIDs returns the NSpid line of /proc/<pid>/status: the pid in each pid
// namespace from the outermost (the caller's) to the innermost.
func nsPIDs(procRoot string, pid int) ([]int, error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/status", procRoot, pid))
	if err != nil {
		return nil, err
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("NSpid:")) {
			continue
		}
		var out []int
		for _, f := range strings.Fields(string(line[len("NSpid:"):])) {
			n, err := strconv.Atoi(f)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, nil
	}
	return []int{pid}, nil // kernels without NSpid: no pid namespaces
}

// startTime returns field 22 of /proc/<pid>/stat (clock ticks since boot).
func startTime(procRoot string, pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", procRoot, pid))
	if err != nil {
		return 0, err
	}
	// comm (field 2) may contain spaces and parentheses: skip to the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, fmt.Errorf("procctl: malformed stat for pid %d", pid)
	}
	f := strings.Fields(string(b[i+1:]))
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return 0, fmt.Errorf("procctl: short stat for pid %d", pid)
	}
	return strconv.ParseUint(f[19], 10, 64)
}
