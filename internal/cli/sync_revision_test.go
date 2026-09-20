package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSyncCompletionRejectsPendingZeroByteItems(t *testing.T) {
	for _, field := range []string{"needItems", "needDeletes"} {
		t.Run(field, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"completion":100,"needBytes":0,"%s":1}`, field)
			}))
			defer srv.Close()
			pct, need, err := syncthingCompletion(context.Background(), srv.URL, "key", "folder", "peer")
			if err != nil {
				t.Fatal(err)
			}
			if syncthingInitialSyncComplete(pct, need, 100, 0) {
				t.Fatal("pending zero-byte items reported as converged")
			}
		})
	}
}

func TestSyncScanSettledRejectsQueuedAndErrorStates(t *testing.T) {
	for _, state := range []string{"scan-waiting", "scan-preparing", "error", "unknown"} {
		t.Run(state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"state":%q}`, state) }))
			defer srv.Close()
			settled, err := syncthingFolderScanSettled(context.Background(), srv.URL, "key", srv.URL, "key", "folder")
			if err != nil {
				t.Fatal(err)
			}
			if settled {
				t.Fatalf("%s reported as settled", state)
			}
		})
	}
}

func TestSyncRevisionRejectsStaleOrDisconnectedPeer(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		seq         int64
		items       int
		want        bool
	}{
		{"current", "valid", 10, 0, true},
		{"stale index same file counts", "valid", 9, 0, false},
		{"disconnected zero queue", "unknown", 10, 0, false},
		{"not sharing", "notSharing", 10, 0, false},
		{"empty directory pending", "valid", 10, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/rest/db/status":
					fmt.Fprint(w, `{"state":"idle","sequence":10,"localFiles":1,"globalFiles":1}`)
				case "/rest/db/completion":
					fmt.Fprintf(w, `{"completion":100,"sequence":%d,"remoteState":%q,"needItems":%d}`, tc.seq, tc.state, tc.items)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			_, reason, err := syncthingRevisionConvergence(context.Background(), srv.URL, "key", srv.URL, "key", "local", "remote", "folder")
			if err != nil {
				t.Fatal(err)
			}
			if (reason == "") != tc.want {
				t.Fatalf("converged=%v reason=%s", tc.want, reason)
			}
		})
	}
}

func TestSyncRevisionDetectsEditDuringVerification(t *testing.T) {
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/db/status" {
			sequence := 10
			if reads.Add(1) > 2 {
				sequence = 11
			}
			fmt.Fprintf(w, `{"state":"idle","sequence":%d}`, sequence)
		} else {
			fmt.Fprint(w, `{"completion":100,"sequence":10,"remoteState":"valid"}`)
		}
	}))
	defer srv.Close()
	_, reason, err := syncthingRevisionConvergence(context.Background(), srv.URL, "key", srv.URL, "key", "local", "remote", "folder")
	if err != nil || !strings.Contains(reason, "changed during") {
		t.Fatalf("err=%v reason=%s", err, reason)
	}
}

func TestSyncWaitScanOutlivesHTTPTimeoutButNotOverallDeadline(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			previous := syncthingHTTPClient
			syncthingHTTPClient = &http.Client{Timeout: 10 * time.Millisecond}
			defer func() { syncthingHTTPClient = previous }()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/rest/db/scan" {
					if !complete {
						<-r.Context().Done()
						return
					}
					time.Sleep(35 * time.Millisecond)
					fmt.Fprint(w, `{}`)
				} else if r.URL.Path == "/rest/db/status" {
					fmt.Fprint(w, `{"state":"idle","sequence":10}`)
				} else {
					fmt.Fprint(w, `{"completion":100,"sequence":10,"remoteState":"valid"}`)
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			var out bytes.Buffer
			err := waitSyncthingRevisions(ctx, srv.URL, "key", srv.URL, "key", "local", "remote", []syncFolder{{id: "folder"}}, &out)
			if complete && err != nil {
				t.Fatal(err)
			}
			if !complete && (!errors.Is(err, context.DeadlineExceeded) || strings.Contains(out.String(), "Sync converged")) {
				t.Fatalf("err=%v out=%s", err, out.String())
			}
		})
	}
}

func TestSyncWaitNeverSucceedsOnMissingRevision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"state":"idle","completion":100,"remoteState":"valid"}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := waitSyncthingRevisions(ctx, srv.URL, "key", srv.URL, "key", "local", "remote", []syncFolder{{id: "folder"}}, &out)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(out.String(), "Sync converged") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
}

func TestSyncRevisionComparesEachDeviceWithPeerView(t *testing.T) {
	server := func(own, peer int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/rest/db/status" {
				fmt.Fprintf(w, `{"state":"idle","sequence":%d}`, own)
			} else {
				fmt.Fprintf(w, `{"completion":100,"sequence":%d,"remoteState":"valid"}`, peer)
			}
		}))
	}
	local := server(10, 50)
	defer local.Close()
	remote := server(50, 10)
	defer remote.Close()
	_, reason, err := syncthingRevisionConvergence(context.Background(), local.URL, "key", remote.URL, "key", "local", "remote", "folder")
	if err != nil || reason != "" {
		t.Fatalf("independent sequence numbers rejected: err=%v reason=%s", err, reason)
	}
}

func TestSyncWaitChecksEveryMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/db/scan":
			fmt.Fprint(w, `{}`)
		case "/rest/db/status":
			fmt.Fprint(w, `{"state":"idle","sequence":10}`)
		default:
			seq := 10
			if r.URL.Query().Get("folder") == "extra" {
				seq = 9
			}
			fmt.Fprintf(w, `{"completion":100,"sequence":%d,"remoteState":"valid"}`, seq)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := waitSyncthingRevisions(ctx, srv.URL, "key", srv.URL, "key", "local", "remote", []syncFolder{{id: "primary"}, {id: "extra"}}, &out)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(out.String(), "Sync converged") || !strings.Contains(out.String(), "extra: peer index") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
}
