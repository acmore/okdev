package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	glssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/acmore/okdev/internal/fanout"
)

// startTestSSHD boots a real okdev-sshd server (the same newServer used in
// production) on a random loopback port, authorized for pub. It returns the
// port. The integration tests below exercise the full driver↔sshd path over
// real SSH connections.
func startTestSSHD(t *testing.T, pub gossh.PublicKey) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newServer(ln.Addr().String(), "/bin/sh", []glssh.PublicKey{pub})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// newTestKeypair generates an ed25519 keypair, writes the private key in
// OpenSSH PEM format to dir, and returns (keyPath, publicKey).
func newTestKeypair(t *testing.T, dir string) (string, gossh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "okdev-test")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("convert public key: %v", err)
	}
	return keyPath, sshPub
}

// runFanoutRequest drives runFanout exactly as the CLI does: request JSON on
// stdin, frames parsed from stdout.
func runFanoutRequest(t *testing.T, req fanout.Request) (*fanout.Stream, string, int) {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := runFanout(bytes.NewReader(payload), &stdout, &stderr)
	stream, err := fanout.ParseStream(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		t.Fatalf("parse stream: %v\nstdout: %s", err, stdout.String())
	}
	return stream, stderr.String(), code
}

func baseRequest(keyPath string, targets []fanout.Target) fanout.Request {
	return fanout.Request{
		Version: fanout.ProtocolVersion,
		User:    "tester",
		KeyPath: keyPath,
		Port:    0, // per-target ports are joined below; tests use Addr "127.0.0.1" + per-target port via Port field
		Targets: targets,
		Retries: 1,
	}
}

func resultByPod(stream *fanout.Stream) map[string]fanout.Result {
	byPod := make(map[string]fanout.Result, len(stream.Results))
	for _, r := range stream.Results {
		byPod[r.Pod] = r
	}
	return byPod
}

func TestFanoutIntegrationCommandAcrossServers(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := newTestKeypair(t, dir)

	// Three independent sshd instances stand in for three pods. They share
	// one port only per request, so give each its own request... instead,
	// all targets share the request-level Port; run three servers and pick
	// one per subtest would lose the fanout aspect. Real pods all listen on
	// 2222; loopback servers cannot share a port, so run one server and
	// address it via three target entries to still exercise concurrent
	// sessions and per-pod frame accounting.
	port := startTestSSHD(t, pub)

	req := baseRequest(keyPath, []fanout.Target{
		{Pod: "pod-a", Addr: "127.0.0.1"},
		{Pod: "pod-b", Addr: "127.0.0.1"},
		{Pod: "pod-c", Addr: "127.0.0.1"},
	})
	req.Port = port
	req.Command = "printf '%s' \"hello-$OKDEV_FANOUT_TEST\"; printf 'warn' >&2"

	stream, stderr, code := runFanoutRequest(t, req)
	if code != 0 {
		t.Fatalf("runFanout exit %d, stderr: %s", code, stderr)
	}
	if !stream.HelloSeen || !stream.DoneSeen || stream.DoneCount != 3 {
		t.Fatalf("expected hello + done(3), got hello=%v done=%v count=%d", stream.HelloSeen, stream.DoneSeen, stream.DoneCount)
	}
	byPod := resultByPod(stream)
	if len(byPod) != 3 {
		t.Fatalf("expected 3 results, got %d", len(byPod))
	}
	for _, pod := range []string{"pod-a", "pod-b", "pod-c"} {
		r, ok := byPod[pod]
		if !ok {
			t.Fatalf("missing frame for %s", pod)
		}
		if r.Status != fanout.StatusResponded || r.Exit != 0 {
			t.Fatalf("%s: expected responded/0, got %s/%d (err %q)", pod, r.Status, r.Exit, r.Error)
		}
		if got := string(r.Stdout); got != "hello-" {
			t.Fatalf("%s: unexpected stdout %q", pod, got)
		}
		if got := string(r.Stderr); got != "warn" {
			t.Fatalf("%s: unexpected stderr %q", pod, got)
		}
	}
}

func TestFanoutIntegrationRemoteExitIsData(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := newTestKeypair(t, dir)
	port := startTestSSHD(t, pub)

	req := baseRequest(keyPath, []fanout.Target{{Pod: "pod-a", Addr: "127.0.0.1"}})
	req.Port = port
	req.Command = "exit 7"

	stream, stderr, code := runFanoutRequest(t, req)
	if code != 0 {
		t.Fatalf("runFanout exit %d, stderr: %s", code, stderr)
	}
	r := resultByPod(stream)["pod-a"]
	if r.Status != fanout.StatusResponded || r.Exit != 7 || r.Error != "" {
		t.Fatalf("expected responded/7 with no error, got %s/%d (err %q)", r.Status, r.Exit, r.Error)
	}
}

func TestFanoutIntegrationUnreachableRetriesAndReports(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := newTestKeypair(t, dir)
	port := startTestSSHD(t, pub)

	// Reserve a port with no listener for the unreachable pod.
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closedLn.Addr().(*net.TCPAddr).Port
	_ = closedLn.Close()

	// Both targets share the request-level port, so aim the unreachable pod
	// at the closed port via a dedicated request.
	reqDown := baseRequest(keyPath, []fanout.Target{{Pod: "pod-down", Addr: "127.0.0.1"}})
	reqDown.Port = closedPort
	reqDown.Retries = 2
	reqDown.Command = "true"

	stream, _, code := runFanoutRequest(t, reqDown)
	if code != 0 {
		t.Fatalf("runFanout exit %d", code)
	}
	r := resultByPod(stream)["pod-down"]
	if r.Status != fanout.StatusUnreachable || r.Exit != -1 {
		t.Fatalf("expected unreachable/-1, got %s/%d", r.Status, r.Exit)
	}
	if r.Attempts != 3 {
		t.Fatalf("expected 3 dial attempts (1 + 2 retries), got %d", r.Attempts)
	}

	// The healthy server still lists fine in the same run shape.
	reqUp := baseRequest(keyPath, []fanout.Target{{Pod: "pod-up", Addr: "127.0.0.1"}})
	reqUp.Port = port
	reqUp.Command = "true"
	stream, _, _ = runFanoutRequest(t, reqUp)
	if r := resultByPod(stream)["pod-up"]; r.Status != fanout.StatusResponded {
		t.Fatalf("healthy pod: expected responded, got %s (%s)", r.Status, r.Error)
	}
}

func TestFanoutIntegrationScriptModeHonorsShebangAndArgs(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := newTestKeypair(t, dir)
	port := startTestSSHD(t, pub)

	req := baseRequest(keyPath, []fanout.Target{{Pod: "pod-a", Addr: "127.0.0.1"}})
	req.Port = port
	req.Script = []byte("#!/bin/sh\nprintf 'arg1=%s argc=%s' \"$1\" \"$#\"\n")
	req.ScriptArgs = []string{"with space", "two"}

	stream, stderr, code := runFanoutRequest(t, req)
	if code != 0 {
		t.Fatalf("runFanout exit %d, stderr: %s", code, stderr)
	}
	r := resultByPod(stream)["pod-a"]
	if r.Status != fanout.StatusResponded || r.Exit != 0 {
		t.Fatalf("expected responded/0, got %s/%d (err %q)", r.Status, r.Exit, r.Error)
	}
	if got := string(r.Stdout); got != "arg1=with space argc=2" {
		t.Fatalf("unexpected stdout %q", got)
	}
}

func TestFanoutIntegrationPerPodTimeout(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := newTestKeypair(t, dir)
	port := startTestSSHD(t, pub)

	req := baseRequest(keyPath, []fanout.Target{{Pod: "pod-slow", Addr: "127.0.0.1"}})
	req.Port = port
	req.TimeoutSec = 1
	req.Command = "sleep 30"

	start := time.Now()
	stream, _, code := runFanoutRequest(t, req)
	if code != 0 {
		t.Fatalf("runFanout exit %d", code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout did not bound the run: %s", elapsed)
	}
	r := resultByPod(stream)["pod-slow"]
	if r.Status != fanout.StatusTimeout || r.Exit != -1 {
		t.Fatalf("expected timeout/-1, got %s/%d", r.Status, r.Exit)
	}
}

func TestFanoutIntegrationBinaryAndMultilineOutputSurvivesFraming(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := newTestKeypair(t, dir)
	port := startTestSSHD(t, pub)

	req := baseRequest(keyPath, []fanout.Target{{Pod: "pod-a", Addr: "127.0.0.1"}})
	req.Port = port
	// Output includes newlines, a fake frame prefix, and non-UTF8 bytes: none
	// of it may corrupt frame parsing.
	req.Command = `printf 'line1\nline2\n__OKDEV_F1__ not-a-frame\n'; printf '\xff\xfe'`

	stream, _, code := runFanoutRequest(t, req)
	if code != 0 {
		t.Fatalf("runFanout exit %d", code)
	}
	r := resultByPod(stream)["pod-a"]
	if r.Status != fanout.StatusResponded {
		t.Fatalf("expected responded, got %s (%s)", r.Status, r.Error)
	}
	want := "line1\nline2\n__OKDEV_F1__ not-a-frame\n\xff\xfe"
	if got := string(r.Stdout); got != want {
		t.Fatalf("stdout corrupted by framing:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(strconv.Quote(string(r.Stdout)), `\xff\xfe`) {
		t.Fatalf("binary bytes lost: %q", r.Stdout)
	}
}

func TestFanoutIntegrationBadKeyFailsBeforeAnyRun(t *testing.T) {
	dir := t.TempDir()
	_, pub := newTestKeypair(t, dir)
	port := startTestSSHD(t, pub)

	req := baseRequest(filepath.Join(dir, "missing_key"), []fanout.Target{{Pod: "pod-a", Addr: "127.0.0.1"}})
	req.Port = port
	req.Command = "true"

	stream, stderr, code := runFanoutRequest(t, req)
	if code == 0 {
		t.Fatalf("expected non-zero exit for unreadable key")
	}
	if len(stream.Results) != 0 || stream.DoneSeen {
		t.Fatalf("no pod may run when the key is unreadable: %+v", stream)
	}
	if !strings.Contains(stderr, "read key") {
		t.Fatalf("expected key error on stderr, got %q", stderr)
	}
}
