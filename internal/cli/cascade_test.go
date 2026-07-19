package cli

import (
	"strings"
	"testing"
)

func TestDetachWrapperCascadeBlock(t *testing.T) {
	withPeers := detachWrapperScript("job-1", []string{"sleep", "9"}, nil, "/var/okdev/exec/job-1.json", "RUN", "DONE", []string{"pod-b", "pod-c"})
	for _, want := range []string{"'pod-b' 'pod-c'", "OKDEV_CASCADE_KILL", "BatchMode=yes", "ConnectTimeout=5"} {
		if !strings.Contains(withPeers, want) {
			t.Fatalf("cascade wrapper missing %q:\n%s", want, withPeers)
		}
	}
	if !strings.Contains(withPeers, "cascade; cleanup;") {
		t.Fatalf("trap must cascade before cleanup:\n%s", withPeers)
	}

	without := detachWrapperScript("job-1", []string{"sleep", "9"}, nil, "/var/okdev/exec/job-1.json", "RUN", "DONE", nil)
	if strings.Contains(without, "OKDEV_CASCADE_KILL") || strings.Contains(without, "ssh -o") {
		t.Fatalf("no-cascade wrapper must not carry ssh payload:\n%s", without)
	}
	if !strings.Contains(without, "cascade() { :; }") {
		t.Fatalf("no-cascade wrapper needs the no-op cascade for the shared trap:\n%s", without)
	}
}

func TestCascadePeerKillScriptShape(t *testing.T) {
	script := cascadePeerKillScript("job-abc")
	for _, want := range []string{
		"'OKDEV_JOB_ID=job-abc'",           // environ verification against pgid reuse
		`[ "$state" = "running" ] || exit`, // exited/orphaned peers left alone
		`kill -9 "-$pgid"`,                 // group kill
		".cascaded",                        // forensics marker
		"/var/okdev/exec",
		"/tmp/okdev-exec", // legacy dir fallback
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("kill script missing %q:\n%s", want, script)
		}
	}
}

func TestSummarizeLogicalJobStateDegraded(t *testing.T) {
	one := 1
	rows := []execJobView{
		{State: "running"},
		{State: "exited", ExitCode: &one},
		{State: "running"},
	}
	if got := summarizeLogicalJobState(rows); got != "degraded(2 running, 1 failed of 3)" {
		t.Fatalf("summary = %q", got)
	}
	zero := 0
	if got := summarizeLogicalJobState([]execJobView{{State: "running"}, {State: "exited", ExitCode: &zero}}); got != "running(1/2)" {
		t.Fatalf("clean partial completion is not degraded, got %q", got)
	}
	if got := summarizeLogicalJobState([]execJobView{{State: "running"}, {State: "running"}}); got != "running(2/2)" {
		t.Fatalf("all-running unchanged, got %q", got)
	}
}
