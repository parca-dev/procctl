package e2e

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// S8: hostile clients. Every broken region is rejected for its reason, or,
// when the region itself is valid but its state block is not, attached and
// isolated (its reads fail, nobody else notices). The agent never crashes and
// well-behaved clients are unaffected.
func TestS8Hostile(t *testing.T) {
	t.Parallel()
	rejected := map[string]string{ // mode → rejection reason (substring)
		"no-seals":          "missing seals",
		"grow-seal-missing": "missing seals",
		"write-sealed":      "write-sealed",
		"wrong-size":        "total_size",
		"spoof-name":        "does not match name",
		"bad-crc":           "header crc mismatch",
		"bad-offsets":       "agent_area_off",
		"huge-len":          "bad state_size",
		"no-state":          "no state block",
	}
	isolated := []string{"bad-state-addr", "state-never-even"} // attached; state reads fail
	e := newScenario(t, "s8")
	e.agent(agentDS, nil)
	good := e.clients(clientDP, 2)

	// One deployment per mode, rolled out concurrently.
	modes := append(append([]string{"spoof-pid"}, isolated...), keys(rejected)...)
	var mu sync.Mutex
	var wg sync.WaitGroup
	spoofPod := ""
	for _, m := range modes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ps, err := e.clientsWithin(120*time.Second, "hostile-"+m, 1, nil, "--hostile="+m)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("hostile %s: %v", m, err)
			} else if m == "spoof-pid" {
				spoofPod = ps[0]
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	s := e.wait(30*time.Second, "every hostile region seen", func(s summary) bool {
		for _, reason := range rejected {
			if !hasReason(s, reason) {
				return false
			}
		}
		isolatedSeen := 0
		for _, c := range s.Clients {
			if c.Pod == "" && c.ReadErrs > 0 {
				isolatedSeen++
			}
		}
		_, spoofSeen := byPod(s)[spoofPod]
		return isolatedSeen == len(isolated) && spoofSeen && allAttached(good)(s)
	})
	// spoof-pid: the header lies about the pid; the agent uses the pid it found
	// the fd under, which checkHealthy (S15) compares with the client's getpid().
	checkHealthy(t, s)
	if want := len(good) + 1 + len(isolated); s.Attached != want {
		var who []string
		for _, c := range s.Clients {
			who = append(who, fmt.Sprintf("%s(pid %d)", c.Pod, c.HostPID))
		}
		t.Errorf("%d attached, want %d (good + spoof-pid + isolated): %v", s.Attached, want, who)
	}
	time.Sleep(3 * time.Second) // more rescans and reads of the hostile regions
	s, _ = e.latest()
	checkHealthy(t, s)
	if !allAttached(good)(s) {
		t.Error("well-behaved clients affected")
	}
	if n := e.restarts(e.agentNS, "app="+agentDS); n != 0 {
		t.Errorf("agent restarted %d times", n)
	}
	t.Logf("rejections: %v", s.Rejected)
}

func hasReason(s summary, substr string) bool {
	for r, n := range s.Rejected {
		if n > 0 && strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
