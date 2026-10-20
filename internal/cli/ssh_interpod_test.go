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
	}, false)

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

	err := configureInterPodSSHOnPods(context.Background(), client, "default", "pytorch", endpoints, "PRIVATE KEY", "PUBLIC KEY", "root", false)
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

const testHostKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
const testHostKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIB7EX9Bd3RCSaxHdWSkb4aMH/LkUBnI1UrgZKNNA+hj5"

type fakeHostKeyProbe struct {
	out map[string]string
}

func (f fakeHostKeyProbe) ExecShInContainer(_ context.Context, _, pod, _, script string) ([]byte, error) {
	if !strings.Contains(script, "--ensure-host-key") {
		return nil, nil
	}
	return []byte(f.out[pod]), nil
}

func TestBuildInterPodSSHConfigPinsHostKeysByPodName(t *testing.T) {
	cfg := buildInterPodSSHConfig("root", []interPodSSHEndpoint{
		{PodName: "trainer-master-0", PodIP: "10.0.0.11"},
	}, true)
	for _, want := range []string{
		"StrictHostKeyChecking yes",
		"UserKnownHostsFile ~/.ssh/okdev_interpod_known_hosts",
		// Keyed by pod name: an IP a recreated or foreign pod now holds
		// must not inherit the old pod's trust.
		"HostKeyAlias trainer-master-0",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("pinned config missing %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "StrictHostKeyChecking no") || strings.Contains(cfg, "/dev/null") {
		t.Fatalf("pinned config must not disable host key checks:\n%s", cfg)
	}
}

func TestCollectInterPodHostKeys(t *testing.T) {
	endpoints := []interPodSSHEndpoint{{PodName: "a"}, {PodName: "b"}}
	pinned, err := collectInterPodHostKeys(context.Background(), fakeHostKeyProbe{out: map[string]string{
		"a": testHostKeyA + "\n", "b": testHostKeyB + "\n",
	}}, "ns", "dev", endpoints)
	if err != nil || !pinned || endpoints[0].HostKey != testHostKeyA || endpoints[1].HostKey != testHostKeyB {
		t.Fatalf("expected both keys pinned, got pinned=%v err=%v %+v", pinned, err, endpoints)
	}
	if got := buildInterPodKnownHosts(endpoints); got != "a "+testHostKeyA+"\nb "+testHostKeyB+"\n" {
		t.Fatalf("unexpected known_hosts:\n%s", got)
	}

	// One sidecar predating pinning: the session falls back as a whole.
	pinned, err = collectInterPodHostKeys(context.Background(), fakeHostKeyProbe{out: map[string]string{
		"a": testHostKeyA, "b": hostKeyUnsupportedToken + "\n",
	}}, "ns", "dev", []interPodSSHEndpoint{{PodName: "a"}, {PodName: "b"}})
	if err != nil || pinned {
		t.Fatalf("expected legacy fallback, got pinned=%v err=%v", pinned, err)
	}

	// Anything else is an error, never a silent downgrade.
	if _, err := collectInterPodHostKeys(context.Background(), fakeHostKeyProbe{out: map[string]string{
		"a": "garbage",
	}}, "ns", "dev", []interPodSSHEndpoint{{PodName: "a"}}); err == nil {
		t.Fatal("unparseable host key must fail setup")
	}
}

func TestInterPodSetupScriptWritesOrClearsKnownHosts(t *testing.T) {
	pinned := buildInterPodSSHSetupScript("PUB", "CFG", "a "+testHostKeyA+"\n")
	if !strings.Contains(pinned, "okdev_interpod_known_hosts") || !strings.Contains(pinned, testHostKeyA) {
		t.Fatalf("pinned setup must write known_hosts:\n%s", pinned)
	}
	legacy := buildInterPodSSHSetupScript("PUB", "CFG", "")
	if !strings.Contains(legacy, `rm -f "$HOME/.ssh/okdev_interpod_known_hosts"`) {
		t.Fatalf("legacy setup must remove stale pinned keys:\n%s", legacy)
	}
}
