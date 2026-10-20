package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// S8: hostile clients. Every broken region is rejected for its reason, or,
// when the region itself is valid but its state block is not, attached and
// isolated (its reads fail, nobody else notices). The agent never crashes and
// well-behaved clients are unaffected.
func TestS8Hostile(t *testing.T) {
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
	reset(t)
	deployAgent(t)
	deployClients(t, 2)
	good := pods(t, clientNS, "app="+clientDP)

	hostilePods := map[string]string{} // pod → mode
	modes := append(append([]string{"spoof-pid"}, isolated...), keys(rejected)...)
	for _, m := range modes {
		ps, err := deployVariant(t, "hostile-"+m, 1, []string{"--hostile=" + m})
		if err != nil {
			t.Fatalf("hostile %s: %v", m, err)
		}
		hostilePods[ps[0]] = m
	}
	s := waitSummary(t, 30*time.Second, "every hostile region seen", func(s summary) bool {
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
		_, spoofSeen := byPod(s)[podFor(hostilePods, "spoof-pid")]
		return isolatedSeen == len(isolated) && spoofSeen && allAttached(t, good)(s)
	})
	for m, reason := range rejected {
		t.Logf("%-18s rejected: %q", m, reason)
	}
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
	time.Sleep(5 * time.Second) // several rescans and reads of the hostile regions
	s, _ = latestSummary(t)
	checkHealthy(t, s)
	if !allAttached(t, good)(s) {
		t.Error("well-behaved clients affected")
	}
	if n := restarts(t, agentNS, "app="+agentDS); n != 0 {
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

func podFor(m map[string]string, mode string) string {
	for p, v := range m {
		if v == mode {
			return p
		}
	}
	return ""
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
