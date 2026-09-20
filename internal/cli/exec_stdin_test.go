package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/acmore/okdev/internal/kube"
	"io"
	k8sexec "k8s.io/client-go/util/exec"
	"strings"
	"testing"
	"time"
)

type unreadableExecInput struct{ t *testing.T }

func (r unreadableExecInput) Read([]byte) (int, error) {
	r.t.Fatal("validation consumed stdin")
	return 0, io.EOF
}

func TestExecStdinValidationBeforeClusterAccess(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-i", "--detach", "--", "cat"}, "--stdin cannot be used with --detach"},
		{[]string{"--stdin", "--json", "--", "cat"}, "--stdin cannot be used with --json"},
		{[]string{"-i", "--script", "/does/not/exist"}, "--stdin cannot be used with --script"},
		{[]string{"-i", "--log-dir", "/does/not/exist", "--", "cat"}, "--stdin cannot be used with --log-dir"},
		{[]string{"-i"}, "--stdin requires a command after --"},
		{[]string{"-i", "--all", "--", "cat"}, "--stdin requires the target pod or one explicit --pod"},
		{[]string{"-i", "--pod", "a,b", "--", "cat"}, "--stdin requires the target pod or one explicit --pod"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd := newExecCmd(&Options{ConfigPath: "/does/not/exist"})
			cmd.SetIn(unreadableExecInput{t})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

type stdinExecClient struct {
	t     *testing.T
	calls int
	run   func(context.Context, io.Reader, io.Writer, io.Writer) error
}

func (c *stdinExecClient) ExecInteractive(ctx context.Context, ns, pod string, tty bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	c.t.Fatal("container targeting lost")
	return nil
}
func (c *stdinExecClient) ExecInteractiveInContainer(ctx context.Context, ns, pod, container string, tty bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	c.calls++
	if tty || ns != "team" || pod != "target" || container != "dev" || len(command) != 1 || command[0] != "cat" {
		c.t.Fatalf("wrong target or TTY: %s/%s/%s tty=%v command=%v", ns, pod, container, tty, command)
	}
	return c.run(ctx, stdin, stdout, stderr)
}
func stdinTestGroups() []execPodGroup {
	return []execPodGroup{{Pods: []kube.PodSummary{{Name: "target"}}}}
}

func TestExecStdinPreservesBytesAndEOF(t *testing.T) {
	for _, input := range [][]byte{nil, []byte("hello\n"), {0, 255, 1, 128, 10, 0}} {
		t.Run(string(input), func(t *testing.T) {
			var out, stderr bytes.Buffer
			c := &stdinExecClient{t: t, run: func(_ context.Context, in io.Reader, out, errOut io.Writer) error {
				_, err := io.Copy(out, in)
				_, _ = io.WriteString(errOut, "diagnostic")
				return err
			}}
			err := runExecStdin(context.Background(), c, "team", stdinTestGroups(), "dev", []string{"cat"}, 0, bytes.NewReader(input), &out, &stderr)
			if err != nil || !bytes.Equal(out.Bytes(), input) || stderr.String() != "diagnostic" || c.calls != 1 {
				t.Fatalf("err=%v output=%v stderr=%q calls=%d", err, out.Bytes(), stderr.String(), c.calls)
			}
		})
	}
}

func TestExecStdinDoesNotReplayConsumedInput(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, errors.New("unable to upgrade connection"), k8sexec.CodeExitError{Err: errors.New("exit 7"), Code: 7}} {
		c := &stdinExecClient{t: t, run: func(_ context.Context, in io.Reader, _, _ io.Writer) error { _, _ = io.ReadAll(in); return failure }}
		err := runExecStdin(context.Background(), c, "team", stdinTestGroups(), "dev", []string{"cat"}, 0, strings.NewReader("once"), io.Discard, io.Discard)
		if !errors.Is(err, failure) || c.calls != 1 {
			t.Fatalf("failure=%v err=%v attempts=%d", failure, err, c.calls)
		}
		var remote k8sexec.ExitError
		if !errors.As(failure, &remote) {
			if code, ok := ClassifiedExitCode(err); !ok || code != 69 {
				t.Fatalf("stream failure exit code=%d classified=%v", code, ok)
			}
		}
	}
}

func TestExecStdinCancellationAndTimeout(t *testing.T) {
	for _, cancelParent := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		c := &stdinExecClient{t: t, run: func(ctx context.Context, _ io.Reader, _, _ io.Writer) error {
			if cancelParent {
				cancel()
			}
			<-ctx.Done()
			return ctx.Err()
		}}
		err := runExecStdin(ctx, c, "team", stdinTestGroups(), "dev", []string{"cat"}, 30*time.Millisecond, unreadableExecInput{t}, io.Discard, io.Discard)
		cancel()
		want := context.DeadlineExceeded
		if cancelParent {
			want = context.Canceled
		}
		if !errors.Is(err, want) || c.calls != 1 {
			t.Fatalf("err=%v calls=%d", err, c.calls)
		}
	}
}

func TestExecStdinRejectsMultipleTargetsWithoutReading(t *testing.T) {
	groups := stdinTestGroups()
	groups[0].Pods = append(groups[0].Pods, kube.PodSummary{Name: "other"})
	c := &stdinExecClient{t: t}
	err := runExecStdin(context.Background(), c, "team", groups, "dev", []string{"cat"}, 0, unreadableExecInput{t}, io.Discard, io.Discard)
	if err == nil || c.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, c.calls)
	}
}

func TestExecStdinStreamsBeforeEOF(t *testing.T) {
	in, writer := io.Pipe()
	defer in.Close()
	defer writer.Close()
	received := make(chan struct{})
	c := &stdinExecClient{t: t, run: func(_ context.Context, r io.Reader, _, _ io.Writer) error {
		first := make([]byte, 5)
		if _, err := io.ReadFull(r, first); err != nil {
			return err
		}
		close(received)
		_, err := io.Copy(io.Discard, r)
		return err
	}}
	done := make(chan error, 1)
	go func() {
		done <- runExecStdin(context.Background(), c, "team", stdinTestGroups(), "dev", []string{"cat"}, 0, in, io.Discard, io.Discard)
	}()
	go func() { _, _ = io.WriteString(writer, "hello") }()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("input buffered until EOF")
	}
	writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
