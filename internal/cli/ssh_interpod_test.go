package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

type fakeInterPodSSHClient struct {
	calls []interPodSSHInstallCall
}

type interPodSSHInstallCall struct {
	namespace string
	pod       string
	container string
	argv      string
	script    string
	stdin     string
}

func (f *fakeInterPodSSHClient) ExecInteractiveInContainer(_ context.Context, namespace, pod, container string, _ bool, command []string, stdin io.Reader, _, _ io.Writer) error {
	in, _ := io.ReadAll(stdin)
	f.calls = append(f.calls, interPodSSHInstallCall{
		namespace: namespace,
		pod:       pod,
		container: container,
		argv:      strings.Join(command, " "),
		script:    command[len(command)-1],
		stdin:     string(in),
	})
	return nil
}

func TestBuildInterPodSSHConfigIncludesSessionPods(t *testing.T) {
	cfg := buildInterPodSSHConfig("root", []interPodSSHEndpoint{
		{PodName: "trainer-master-0", PodIP: "10.0.0.11"},
		{PodName: "trainer-worker-0", PodIP: "10.0.0.12"},
	})

	for _, want := range []string{
		"Host trainer-master-0",
		"HostName 10.0.0.11",
		"Port 2222",
		"Host trainer-worker-0",
		"HostName 10.0.0.12",
		"IdentityFile ~/.ssh/okdev_interpod_ed25519",
		"StrictHostKeyChecking no",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("expected config to contain %q, got:\n%s", want, cfg)
		}
	}
}

func TestConfigureInterPodSSHOnPodsInstallsKeysAndConfigOnAllPods(t *testing.T) {
	client := &fakeInterPodSSHClient{}
	endpoints := []interPodSSHEndpoint{
		{PodName: "trainer-master-0", PodIP: "10.0.0.11"},
		{PodName: "trainer-worker-0", PodIP: "10.0.0.12"},
		{PodName: "trainer-worker-1", PodIP: "10.0.0.13"},
	}

	err := configureInterPodSSHOnPods(context.Background(), client, "default", "pytorch", endpoints, "PRIVATE KEY", "PUBLIC KEY", "root")
	if err != nil {
		t.Fatalf("configureInterPodSSHOnPods: %v", err)
	}
	if len(client.calls) != len(endpoints) {
		t.Fatalf("expected %d pod setup calls, got %d", len(endpoints), len(client.calls))
	}
	for i, call := range client.calls {
		if call.namespace != "default" {
			t.Fatalf("call %d namespace = %q, want default", i, call.namespace)
		}
		if call.container != "pytorch" {
			t.Fatalf("call %d container = %q, want pytorch", i, call.container)
		}
		// The private key travels on stdin only: exec arguments land in the
		// apiserver request URL (audit logs) and in /proc/<pid>/cmdline.
		if strings.Contains(call.argv, "PRIVATE KEY") {
			t.Fatalf("call %d: private key leaked into exec arguments:\n%s", i, call.argv)
		}
		if call.stdin != "PRIVATE KEY\n" {
			t.Fatalf("call %d: private key must arrive on stdin, got %q", i, call.stdin)
		}
		for _, want := range []string{
			"okdev_interpod_ed25519",
			"okdev_interpod_config",
			"PUBLIC KEY",
			"umask 077",
			"Host trainer-worker-0",
			"Host trainer-worker-1",
		} {
			if !strings.Contains(call.script, want) {
				t.Fatalf("expected setup script for %s to contain %q, got:\n%s", call.pod, want, call.script)
			}
		}
	}
}
