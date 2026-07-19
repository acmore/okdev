package cli

import (
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// A TERM-ignoring process matching the pattern must still be gone after the
// escalation window — the graceful path alone would leave it alive and
// reading as a leak.
func TestPkillByPatternWithEscalationKillsTermIgnorers(t *testing.T) {
	marker := fmt.Sprintf("okdev-test-escalation-%d", time.Now().UnixNano())
	// The marker rides in the cmdline as a comment so pkill/pgrep -f match it.
	victim := exec.Command("sh", "-c", fmt.Sprintf(`trap "" TERM; while :; do sleep 1; done # %s`, marker))
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = victim.Process.Kill() }()
	go func() { _ = victim.Wait() }()

	deadline := time.Now().Add(2 * time.Second)
	for !patternAlive(marker) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !patternAlive(marker) {
		t.Fatal("victim process did not become visible to pgrep")
	}

	if err := pkillByPatternWithEscalation(marker, 1*time.Second); err != nil {
		t.Fatalf("escalation: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for patternAlive(marker) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if patternAlive(marker) {
		t.Fatal("TERM-ignoring process survived the KILL escalation")
	}
}

func TestPkillByPatternWithEscalationNoMatchIsClean(t *testing.T) {
	if err := pkillByPatternWithEscalation("okdev-test-no-such-pattern-xyz", 200*time.Millisecond); err != nil {
		t.Fatalf("no-match must be clean: %v", err)
	}
}
