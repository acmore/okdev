package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/acmore/okdev/internal/fanout"
	"github.com/acmore/okdev/internal/shellutil"
)

const (
	fanoutDefaultConcurrency = 16
	fanoutDialTimeout        = 5 * time.Second
)

// runFanout implements `okdev-sshd fanout`: read a fanout.Request from stdin,
// SSH to every target over the pod network, and emit one result frame per pod
// as it completes. The gateway process runs inside the dev container, so the
// interpod key and the same environment the user's commands expect are both
// present. All frames go to stdout; anything else (diagnostics) goes to
// stderr so the CLI's frame parser never sees it.
func runFanout(stdin io.Reader, stdout, stderr io.Writer) int {
	hello, err := fanout.EncodeHello()
	if err != nil {
		fmt.Fprintf(stderr, "fanout: encode hello: %v\n", err)
		return 1
	}
	var out syncWriter
	out.w = stdout
	out.WriteString(hello)

	var req fanout.Request
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		fmt.Fprintf(stderr, "fanout: read request: %v\n", err)
		return 1
	}
	if err := req.Validate(); err != nil {
		fmt.Fprintf(stderr, "fanout: invalid request: %v\n", err)
		return 1
	}

	key, err := os.ReadFile(expandHome(req.KeyPath))
	if err != nil {
		fmt.Fprintf(stderr, "fanout: read key: %v\n", err)
		return 1
	}
	signer, err := gossh.ParsePrivateKey(key)
	if err != nil {
		fmt.Fprintf(stderr, "fanout: parse key: %v\n", err)
		return 1
	}

	concurrency := req.Fanout
	if concurrency <= 0 {
		concurrency = fanoutDefaultConcurrency
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, target := range req.Targets {
		wg.Add(1)
		go func(target fanout.Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := runFanoutTarget(context.Background(), &req, target, signer)
			frame, err := fanout.EncodeResult(res)
			if err != nil {
				fmt.Fprintf(stderr, "fanout: encode result for %s: %v\n", target.Pod, err)
				return
			}
			out.WriteString(frame)
		}(target)
	}
	wg.Wait()

	done, err := fanout.EncodeDone(len(req.Targets))
	if err != nil {
		fmt.Fprintf(stderr, "fanout: encode done: %v\n", err)
		return 1
	}
	out.WriteString(done)
	return 0
}

// runFanoutTarget runs the request's command on one pod. Only dial failures
// are retried: a dial failure guarantees the command never started, so a
// retry cannot double-execute. Once a session exists, any failure is
// reported as-is.
func runFanoutTarget(ctx context.Context, req *fanout.Request, target fanout.Target, signer gossh.Signer) fanout.Result {
	res := fanout.Result{Pod: target.Pod}
	cfg := &gossh.ClientConfig{
		User:            req.User,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         fanoutDialTimeout,
	}
	addr := net.JoinHostPort(target.Addr, fmt.Sprintf("%d", req.Port))

	var client *gossh.Client
	var dialErr error
	attempts := req.Retries + 1
	for attempt := 1; attempt <= attempts; attempt++ {
		res.Attempts = attempt
		client, dialErr = gossh.Dial("tcp", addr, cfg)
		if dialErr == nil {
			break
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
	}
	if dialErr != nil {
		res.Status = fanout.StatusUnreachable
		res.Exit = -1
		res.Error = dialErr.Error()
		return res
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		res.Status = fanout.StatusError
		res.Exit = -1
		res.Error = err.Error()
		return res
	}
	defer session.Close()

	// lockedBuffer, not bytes.Buffer: on the timeout/cancel paths the ssh
	// session's internal copier goroutines may still be writing when we
	// read the partial output.
	var stdoutBuf, stderrBuf lockedBuffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	command := req.Command
	if len(req.Script) > 0 {
		command = scriptOverSSHCommand(req.ScriptArgs)
		session.Stdin = bytes.NewReader(req.Script)
	}

	runErr := make(chan error, 1)
	if err := session.Start(command); err != nil {
		res.Status = fanout.StatusError
		res.Exit = -1
		res.Error = err.Error()
		return res
	}
	go func() { runErr <- session.Wait() }()

	var timeoutCh <-chan time.Time
	if req.TimeoutSec > 0 {
		timer := time.NewTimer(time.Duration(req.TimeoutSec) * time.Second)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case err := <-runErr:
		res.Stdout = stdoutBuf.Bytes()
		res.Stderr = stderrBuf.Bytes()
		if err == nil {
			res.Status = fanout.StatusResponded
			res.Exit = 0
			return res
		}
		var exitErr *gossh.ExitError
		if errors.As(err, &exitErr) {
			// The command ran and reported an exit status: that is data.
			res.Status = fanout.StatusResponded
			res.Exit = exitErr.ExitStatus()
			return res
		}
		// ExitMissingError or a stream failure: the command may have run,
		// so this must never be retried; report the uncertainty.
		res.Status = fanout.StatusError
		res.Exit = -1
		res.Error = err.Error()
		return res
	case <-timeoutCh:
		_ = session.Close()
		_ = client.Close()
		res.Stdout = stdoutBuf.Bytes()
		res.Stderr = stderrBuf.Bytes()
		res.Status = fanout.StatusTimeout
		res.Exit = -1
		res.Error = fmt.Sprintf("timed out after %ds", req.TimeoutSec)
		return res
	case <-ctx.Done():
		_ = session.Close()
		_ = client.Close()
		res.Status = fanout.StatusError
		res.Exit = -1
		res.Error = ctx.Err().Error()
		return res
	}
}

// scriptOverSSHCommand writes the script arriving on stdin to a temp file
// before executing it, so shebang lines are honored exactly like the direct
// exec path's script mode.
func scriptOverSSHCommand(args []string) string {
	quoted := ""
	for _, a := range args {
		quoted += " " + shellutil.Quote(a)
	}
	return `d=$(mktemp -d) && cat >"$d/script" && chmod +x "$d/script" && ` +
		`"$d/script"` + quoted + `; rc=$?; rm -rf "$d"; exit $rc`
}

// expandHome resolves a leading "~/" against the driver's own home
// directory: the driver runs in the dev container, where the interpod
// identity file lives under the user's home.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}

// lockedBuffer is a mutex-guarded bytes.Buffer whose contents can be read
// safely while the ssh session's copier goroutines may still be writing
// (timeout/cancel paths). Bytes returns a copy so callers never alias the
// live buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// syncWriter serializes whole-frame writes from concurrent goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) WriteString(str string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.w, str)
}
