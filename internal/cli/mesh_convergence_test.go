package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestMeshHealthIncludesPendingReceiver(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/pods") {
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"worker-0","uid":"worker-id"},"status":{"phase":"Pending"}}]}`)
			return
		}
		http.Error(w, "hub unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, srv))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	health, err := checkMeshHealth(ctx, &Options{}, &kube.Client{Context: "dev"}, "team", "sess", meshResetLabels("sess"), "hub", "folder")
	if err != nil {
		t.Fatal(err)
	}
	if health == nil || len(health.Receivers) != 1 || health.Receivers[0].Pod != "worker-0" || len(brokenMeshReceiverPods(health)) != 1 {
		t.Fatalf("pending receiver disappeared from expected set: %+v", health)
	}
}

func TestMeshSummaryNeverClaimsUnsyncedReceiversComplete(t *testing.T) {
	summary := &meshSummary{Receivers: []meshReceiverStatus{{Pod: "worker-0", Connected: true}, {Pod: "worker-1", Connected: true, Synced: true}}}
	got := formatMeshSummary(summary)
	if !strings.Contains(got, "1/2") {
		t.Fatalf("partial mesh lacks expected denominator: %s", got)
	}
}

func TestMeshReceiverNeedsCurrentIndexAndLiveConnection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		connected    bool
		peerSequence int
		want         bool
	}{
		{"healthy", true, 10, true}, {"absent worker link", false, 10, false}, {"same counts stale content", true, 9, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/rest/system/connections":
					fmt.Fprintf(w, `{"connections":{"hub":{"connected":%t},"worker":{"connected":%t}}}`, tc.connected, tc.connected)
				case "/rest/db/status":
					fmt.Fprint(w, `{"state":"idle","sequence":10,"localFiles":1,"globalFiles":1}`)
				case "/rest/db/completion":
					fmt.Fprintf(w, `{"remoteState":"valid","sequence":%d,"completion":100,"needBytes":0}`, tc.peerSequence)
				}
			}))
			defer srv.Close()
			h := observeMeshReceiver(context.Background(), "worker-0", srv.URL, "key", srv.URL, "key", "hub", "worker", "folder")
			if h.Err != "" || h.InSync != tc.want || h.Connected != tc.connected {
				t.Fatalf("health=%+v", h)
			}
		})
	}
}

func TestMeshConvergenceTracksPendingMissingAndReplacedReceivers(t *testing.T) {
	before := []kube.PodSummary{{Name: "worker-0", UID: "old"}}
	healthy := []meshReceiverHealth{{Pod: "worker-0", Connected: true, InSync: true}}
	for _, after := range [][]kube.PodSummary{nil, {{Name: "worker-0", UID: "new"}}, {{Name: "worker-0", UID: "old"}, {Name: "worker-1", UID: "added"}}} {
		observed := reconcileMeshObservations(before, after, append([]meshReceiverHealth(nil), healthy...))
		if reason := meshConvergenceReason(&meshHealthSummary{Receivers: observed}, 1); reason == "" {
			t.Fatalf("membership change accepted: %+v", after)
		}
	}
	if meshConvergenceReason(nil, 1) == "" {
		t.Fatal("vanished receiver set accepted")
	}
	if meshConvergenceReason(nil, 0) != "" {
		t.Fatal("single/shared-volume session should not require mesh")
	}
}

func TestSyncReceiverCoverageSeparatesPrimaryAndAdditionalMappings(t *testing.T) {
	hub := meshReceiverHealth{Pod: "hub", Connected: true, InSync: true}
	mesh := &meshHealthSummary{Receivers: []meshReceiverHealth{{Pod: "worker-0", Connected: true, InSync: true}, {Pod: "worker-1", Reason: "not connected"}}}
	primary := buildSyncReceiverCoverage(hub, mesh, true)
	if primary.Expected != 3 || primary.Connected != 2 || primary.Converged != 2 || strings.Join(primary.Pending, ",") != "worker-1" {
		t.Fatalf("coverage=%+v", primary)
	}
	extra := buildSyncReceiverCoverage(hub, mesh, false)
	if extra.Expected != 1 || extra.Converged != 1 || extra.Scope != "target-only" {
		t.Fatalf("extra mapping should not imply worker distribution: %+v", extra)
	}
}

func TestSyncWaitDoesNotSucceedWithHealthyHubAndMissingWorkerLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/db/status" {
			fmt.Fprint(w, `{"state":"idle","sequence":10}`)
		} else if r.URL.Path == "/rest/db/completion" {
			fmt.Fprint(w, `{"remoteState":"valid","sequence":10,"completion":100}`)
		} else {
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var out strings.Builder
	checks := 0
	err := waitSyncthingRevisions(ctx, srv.URL, "key", srv.URL, "key", "local", "hub", []syncFolder{{id: "folder"}}, &out, func(context.Context) error {
		checks++
		return fmt.Errorf("worker-1: link absent")
	})
	if !errors.Is(err, context.DeadlineExceeded) || checks == 0 || strings.Contains(out.String(), "Sync converged") || !strings.Contains(out.String(), "worker-1") {
		t.Fatalf("err=%v checks=%d output=%s", err, checks, out.String())
	}
}

func TestMeshReceiverRejectsUnpublishedReceiveOnlyChanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/connections" {
			fmt.Fprint(w, `{"connections":{"hub":{"connected":true},"worker":{"connected":true}}}`)
		} else if r.URL.Path == "/rest/db/status" {
			fmt.Fprint(w, `{"state":"idle","sequence":10,"receiveOnlyTotalItems":1}`)
		} else {
			fmt.Fprint(w, `{"remoteState":"valid","sequence":10,"completion":100}`)
		}
	}))
	defer srv.Close()
	health := observeMeshReceiver(context.Background(), "worker-0", srv.URL, "key", srv.URL, "key", "hub", "worker", "folder")
	if !health.Connected || health.InSync {
		t.Fatalf("local worker drift accepted: %+v", health)
	}
}

func TestMeshSetupPendingReceiverCannotSucceedAtTimeout(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"worker-0"},"status":{"phase":"Pending"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, srv))
	start := time.Now()
	_, err := setupMesh(context.Background(), &Options{}, &kube.Client{Context: "dev"}, "team", "sess", meshResetLabels("sess"), "hub", "folder", "/workspace", 30*time.Millisecond, nil)
	if err == nil || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("pending receiver setup err=%v elapsed=%s", err, time.Since(start))
	}
}

func TestMeshWaitRetainsInitialReceiverIdentity(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if reads.Add(1) == 1 {
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"worker-0"},"status":{"phase":"Pending"}}]}`)
		} else {
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
		}
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, srv))
	cc := &commandContext{opts: &Options{}, kube: &kube.Client{Context: "dev"}, namespace: "team", sessionName: "sess"}
	check, err := newMeshConvergenceCheck(context.Background(), cc, "hub", "folder", 2)
	if err != nil {
		t.Fatal(err)
	}
	err = check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "worker-0") || !strings.Contains(err.Error(), "at least 2") {
		t.Fatalf("missing worker forgotten: %v", err)
	}
}
