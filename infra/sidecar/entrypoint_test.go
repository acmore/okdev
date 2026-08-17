package sidecar

import (
	"os"
	"strings"
	"testing"
)

// The sidecar's syncthing GUI is also its REST API, and that API is not
// meaningfully authenticated: fetching / hands out a CSRF token that authorizes
// the whole API without the key, so anything that can reach the port can read
// the config (which contains the API key), browse the filesystem, and add a
// folder pointing anywhere. Syncthing runs as root here, so that is arbitrary
// root read/write in the pod.
//
// Kubernetes pod networks are flat and okdev ships no NetworkPolicy, so binding
// this to 0.0.0.0 exposed it to every pod in the cluster. okdev only ever
// reaches it through a port-forward, which enters the pod's own network
// namespace — loopback is all it needs.
func TestSyncthingGUIBindsToLoopbackOnly(t *testing.T) {
	raw, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	script := string(raw)

	if !strings.Contains(script, "--gui-address=http://127.0.0.1:8384") {
		t.Errorf("the syncthing GUI/API must bind loopback:\n%s", script)
	}
	for _, forbidden := range []string{
		"--gui-address=http://0.0.0.0",
		"0.0.0.0:8384",
		"--gui-address=http://[::]",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("%q exposes the unauthenticated syncthing API to the pod network:\n%s", forbidden, script)
		}
	}
}

// The sync protocol port is a different case and must NOT be pinned to
// loopback: mesh sync dials it pod-to-pod as tcp://<PodIP>:22000, and unlike
// the GUI it authenticates peers by device ID over TLS.
func TestSyncProtocolPortIsNotRestrictedToLoopback(t *testing.T) {
	raw, err := os.ReadFile("entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	if strings.Contains(string(raw), "127.0.0.1:22000") {
		t.Fatal("pinning the sync port to loopback breaks pod-to-pod mesh sync")
	}
}
