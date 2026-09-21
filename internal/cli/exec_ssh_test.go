package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/connect"
)

func TestExecSSHFooterPreservesBytesAndEveryStatus(t *testing.T) {
	for code := 0; code <= 255; code++ {
		for _, chunk := range []int{1, 7, 65536} {
			t.Run(fmt.Sprintf("%d/chunk%d", code, chunk), func(t *testing.T) {
				var output bytes.Buffer
				marker := "\x00OKDEV_EXEC_random:"
				payload := append(bytes.Repeat([]byte{0, 1, 255, 'x', '\n'}, 300), []byte(marker+"garbage")...)
				wire := append(append([]byte{}, payload...), []byte(fmt.Sprintf("%s%d\x00", marker, code))...)
				w := &execSSHFooter{out: &output, marker: marker}
				for len(wire) > 0 {
					n := min(chunk, len(wire))
					if _, err := w.Write(wire[:n]); err != nil {
						t.Fatal(err)
					}
					wire = wire[n:]
				}
				got, complete, err := w.finish()
				if err != nil || !complete || got != code || !bytes.Equal(output.Bytes(), payload) {
					t.Fatalf("code=%d complete=%t err=%v corrupted=%t", got, complete, err, !bytes.Equal(output.Bytes(), payload))
				}
			})
		}
	}
}

func TestExecSSHFooterRejectsMissingOrMalformedCompletion(t *testing.T) {
	for _, suffix := range []string{"", "\x00other:0\x00", "\x00mark:256\x00", "\x00mark:-1\x00", "\x00mark:0", "\x00mark:00\x00", "\x00mark:0\x00late"} {
		var out bytes.Buffer
		data := "diagnostic" + suffix
		w := &execSSHFooter{out: &out, marker: "\x00mark:"}
		_, _ = w.Write([]byte(data))
		_, complete, err := w.finish()
		if complete || err != nil || out.String() != data {
			t.Fatalf("accepted or lost bytes: %q", data)
		}
	}
}

func TestExecSSHIdentitySeparatesAllTargetAndCredentialFields(t *testing.T) {
	original := execSSHIdentity{Connection: "cluster", Namespace: "ns", Pod: "pod", UID: "uid", Container: "dev", Started: "time", Key: "key", User: "root", Session: "session", Owner: "owner"}
	for i := 0; i < reflect.TypeFor[execSSHIdentity]().NumField(); i++ {
		changed := original
		field := reflect.ValueOf(&changed).Elem().Field(i)
		field.SetString(field.String() + "changed")
		if changed.digest() == original.digest() {
			t.Fatalf("identity ignores field %d", i)
		}
	}
}

func TestExecSSHFailureClassificationDoesNotConfuseTransport255(t *testing.T) {
	err := &connect.DeliveryError{Err: errors.New("ssh exit status 255: EOF")}
	if kind, code := classifyPodExecFailure(err); kind != "transport" || code != -1 {
		t.Fatalf("%s %d", kind, code)
	}
	if kind, _ := classifyPodExecFailure(&connect.DeliveryError{Err: context.DeadlineExceeded}); kind != "timeout" {
		t.Fatal(kind)
	}
}

func TestExecSSHMissingMasterFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	var out, errOut bytes.Buffer
	err := execSSHRun(context.Background(), t.TempDir()+"/absent.sock", []string{"echo", "must not run"}, nil, &out, &errOut)
	var delivery *connect.DeliveryError
	if !errors.As(err, &delivery) || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%q", err, out.String())
	}
}

func TestExecSSHInvalidModesFailBeforeClusterAccess(t *testing.T) {
	for _, args := range [][]string{{"--transport=bad", "--", "true"}, {"--transport=ssh"}, {"--transport=ssh", "--shell=bash", "--", "true"}, {"--transport=ssh", "--json", "--gateway=p", "--", "true"}} {
		cmd := newExecCmd(&Options{})
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--transport") {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestExecSSHStderrStreamsBeforeCommandCompletion(t *testing.T) {
	var out bytes.Buffer
	w := &execSSHFooter{out: &out, marker: "\x00OKDEV_EXEC_nonce:"}
	if _, err := w.Write([]byte("ready\n")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ready\n" {
		t.Fatalf("stderr was withheld until completion: %q", out.String())
	}
}

func TestStopExecSSHWithoutCacheLeavesHomeUntouched(t *testing.T) {
	home := t.TempDir() + "/" + strings.Repeat("long-home", 15)
	t.Setenv("HOME", home)
	if err := stopExecSSHSession("unused"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("cleanup created home or cache: %v", err)
	}
}
