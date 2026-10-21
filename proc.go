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
