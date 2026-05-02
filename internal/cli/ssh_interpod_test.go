package cli

import (
	"context"
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
	script    string
}

func (f *fakeInterPodSSHClient) ExecShInContainer(_ context.Context, namespace, pod, container, script string) ([]byte, error) {
	f.calls = append(f.calls, interPodSSHInstallCall{
		namespace: namespace,
		pod:       pod,
		container: container,
		script:    script,
	})
	return nil, nil
}

func TestBuildInterPodSSHConfigIncludesSessionPods(t *testing.T) {
	cfg := buildInterPodSSHConfig("root", []interPodSSHEndpoint{
		{PodName: "trainer-master-0", PodIP: "10.0.0.11"},
		{PodName: "trainer-worker-0", PodIP: "10.0.0.12"},
	})

	for _, want := range []string{
		"Host trainer-master-0",
		"HostName 10.0.0.11",
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
		for _, want := range []string{
			"okdev_interpod_ed25519",
			"okdev_interpod_config",
			"PUBLIC KEY",
			"PRIVATE KEY",
			"Host trainer-worker-0",
			"Host trainer-worker-1",
		} {
			if !strings.Contains(call.script, want) {
				t.Fatalf("expected setup script for %s to contain %q, got:\n%s", call.pod, want, call.script)
			}
		}
	}
}
