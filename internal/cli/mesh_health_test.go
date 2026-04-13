package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestBrokenMeshReceiverPodsNilSummary(t *testing.T) {
	if got := brokenMeshReceiverPods(nil); len(got) != 0 {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestBrokenMeshReceiverPodsAllHealthy(t *testing.T) {
	summary := &meshHealthSummary{
		HubPod:   "hub-0",
		FolderID: "okdev-sess1",
		Receivers: []meshReceiverHealth{
			{Pod: "recv-0", Connected: true, InSync: true},
			{Pod: "recv-1", Connected: true, InSync: true},
		},
	}
	if got := brokenMeshReceiverPods(summary); len(got) != 0 {
		t.Fatalf("expected no broken receivers, got %v", got)
	}
}

func TestBrokenMeshReceiverPodsDetectsBroken(t *testing.T) {
	summary := &meshHealthSummary{
		HubPod:   "hub-0",
		FolderID: "okdev-sess1",
		Receivers: []meshReceiverHealth{
			{Pod: "recv-0", Connected: true, InSync: true},
			{Pod: "recv-1", Connected: false, InSync: false},
			{Pod: "recv-2", Connected: true, InSync: false, NeedBytes: 1024},
			{Pod: "recv-3", Err: "port-forward failed"},
		},
	}
	broken := brokenMeshReceiverPods(summary)
	if len(broken) != 3 {
		t.Fatalf("expected 3 broken receivers, got %v", broken)
	}
	expected := map[string]bool{"recv-1": true, "recv-2": true, "recv-3": true}
	for _, name := range broken {
		if !expected[name] {
			t.Fatalf("unexpected broken receiver: %s", name)
		}
	}
}

func TestPrintMeshHealthAllHealthy(t *testing.T) {
	var buf bytes.Buffer
	printMeshHealth(&buf, &meshHealthSummary{
		HubPod:   "hub-0",
		FolderID: "okdev-sess1",
		Receivers: []meshReceiverHealth{
			{Pod: "recv-0", Connected: true, InSync: true},
		},
	})
	got := buf.String()
	if !strings.Contains(got, "1/1 receiver(s) healthy") {
		t.Fatalf("expected healthy summary, got %q", got)
	}
	if !strings.Contains(got, "recv-0: synced") {
		t.Fatalf("expected recv-0 synced, got %q", got)
	}
}

func TestPrintMeshHealthMixed(t *testing.T) {
	var buf bytes.Buffer
	printMeshHealth(&buf, &meshHealthSummary{
		HubPod:   "hub-0",
		FolderID: "okdev-sess1",
		Receivers: []meshReceiverHealth{
			{Pod: "recv-0", Connected: true, InSync: true},
			{Pod: "recv-1", Connected: false},
			{Pod: "recv-2", Connected: true, InSync: false, NeedBytes: 2048},
			{Pod: "recv-3", Err: "API not ready"},
		},
	})
	got := buf.String()
	if !strings.Contains(got, "1/4 receiver(s) healthy") {
		t.Fatalf("expected 1/4 healthy, got %q", got)
	}
	if !strings.Contains(got, "recv-1: disconnected") {
		t.Fatalf("expected recv-1 disconnected, got %q", got)
	}
	if !strings.Contains(got, "recv-2: syncing (2048 bytes remaining)") {
		t.Fatalf("expected recv-2 syncing, got %q", got)
	}
	if !strings.Contains(got, "recv-3: error: API not ready") {
		t.Fatalf("expected recv-3 error, got %q", got)
	}
}

func TestPrintMeshHealthNil(t *testing.T) {
	var buf bytes.Buffer
	printMeshHealth(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("expected empty output for nil health, got %q", buf.String())
	}
}
