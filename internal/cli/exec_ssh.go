package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	k8sexec "k8s.io/client-go/util/exec"
)

const execSSHIdle = "60"

type execSSHIdentity struct {
	Connection, Namespace, Pod, UID, Container, Started, Key, User, Session, Owner string
}

func (id execSSHIdentity) digest() string {
	data, _ := json.Marshal(id)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}

func execSSHStarted(pod kube.PodSummary, container string) (string, error) {
	if pod.UID == "" || pod.Deleting || pod.Phase != "Running" {
		return "", fmt.Errorf("SSH exec requires a live pod: %s", pod.Name)
	}
	for _, c := range pod.ContainerStarts {
		if c.Name == container && !c.StartedAt.IsZero() {
			return c.ContainerID + ":" + c.StartedAt.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", fmt.Errorf("SSH exec requires running container %s in %s", container, pod.Name)
}

func execSSHDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".okdev", "exec-ssh")
	// OpenSSH appends a random suffix while creating the control socket.
	if len(filepath.Join(dir, strings.Repeat("x", 24)+".sock"))+17 >= 104 {
		return "", errors.New("HOME path is too long for SSH control sockets")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", errors.New("SSH cache directory must be a private directory (mode 0700)")
	}
	return dir, nil
}

func execSSHLock(ctx context.Context, dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

type execSSHClient struct {
	*kube.Client
	namespace, container, keyPath, user, dir string
	identities                               map[string]execSSHIdentity
}

func prepareExecSSH(ctx context.Context, cc *commandContext, groups []execPodGroup, container string) (*execSSHClient, error) {
	if cc.cfg.Spec.AttachOnly != nil {
		return nil, errors.New("--transport=ssh requires a managed okdev session; attach-only uses kubernetes")
	}
	if container == "" || container != resolveTargetContainer(cc.cfg) {
		return nil, errors.New("--transport=ssh only supports the configured dev container")
	}
	if err := ensureCommand("ssh"); err != nil {
		return nil, err
	}
	keyPath, err := defaultSSHKeyPath(cc.cfg)
	if err != nil {
		return nil, err
	}
	keyPath, err = filepath.Abs(keyPath)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read SSH key (run okdev up first): %w", err)
	}
	keyHash := sha256.Sum256(key)
	connection, err := cc.kube.ConnectionIdentity()
	if err != nil {
		return nil, err
	}
	dir, err := execSSHDirectory()
	if err != nil {
		return nil, err
	}
	user := cc.cfg.Spec.SSH.User
	if user == "" {
		user = "root"
	}
	client := &execSSHClient{Client: cc.kube, namespace: cc.namespace, container: container, keyPath: keyPath, user: user, dir: dir, identities: map[string]execSSHIdentity{}}
	for _, group := range groups {
		for _, pod := range group.Pods {
			if _, ok := client.identities[pod.Name]; ok {
				continue
			}
			if err := checkSessionAccessPods(cc.opts, cc.namespace, cc.sessionName, true, []kube.PodSummary{pod}); err != nil {
				return nil, err
			}
			started, err := execSSHStarted(pod, container)
			if err != nil {
				return nil, err
			}
			if err := cc.kube.AuthorizeSSHExec(ctx, cc.namespace, pod.Name); err != nil {
				return nil, err
			}
			client.identities[pod.Name] = execSSHIdentity{Connection: connection, Namespace: cc.namespace, Pod: pod.Name, UID: pod.UID, Container: container, Started: started, Key: hex.EncodeToString(keyHash[:]), User: user, Session: cc.sessionName, Owner: currentOwner(cc.opts)}
		}
	}
	return client, nil
}

func execSSHArgs(socket string) []string {
	return []string{"-F", "/dev/null", "-T", "-S", socket, "-o", "BatchMode=yes", "-o", "ControlMaster=no", "-o", "ProxyCommand=false", "-o", "LogLevel=ERROR", "-o", "SetEnv=OKDEV_NO_TMUX=1"}
}

func execSSHControl(ctx context.Context, socket, operation string) error {
	args := append(execSSHArgs(socket), "-O", operation, "127.0.0.1")
	return exec.CommandContext(ctx, "ssh", args...).Run()
}

func (c *execSSHClient) master(ctx context.Context, id execSSHIdentity) (string, error) {
	socket := filepath.Join(c.dir, id.digest()+".sock")
	metadata := socket + ".json"
	// Warm channels need no creation lock. If down or a disconnect races with
	// this check, delivery fails closed instead of creating a replacement.
	usable := func() bool {
		data, err := os.ReadFile(metadata)
		if err != nil {
			return false
		}
		var saved execSSHIdentity
		return json.Unmarshal(data, &saved) == nil && saved == id && execSSHControl(ctx, socket, "check") == nil
	}
	if usable() {
		return socket, nil
	}
	unlock, err := execSSHLock(ctx, c.dir)
	if err != nil {
		return "", err
	}
	defer unlock()
	if usable() {
		return socket, nil
	}
	_ = execSSHControl(ctx, socket, "exit")
	_ = os.Remove(socket)
	_ = os.Remove(metadata)
	pruneExecSSH(c.dir)
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(id)
	proxy := shellJoinArgv([]string{executable, "exec-ssh-proxy", "--context", c.Context, "--identity", string(data)})
	args := []string{"-F", "/dev/null", "-T", "-M", "-N", "-f", "-S", socket, "-i", c.keyPath,
		"-o", "ControlPersist=" + execSSHIdle, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2",
		"-o", "ProxyCommand=" + strings.ReplaceAll(proxy, "%", "%%"), "-l", c.user, "127.0.0.1"}
	// The detached master retains its diagnostic descriptors. Use a file rather
	// than a pipe so waiting for the bootstrap cannot wait for the master's EOF.
	log, err := os.CreateTemp(c.dir, "bootstrap-")
	if err != nil {
		return "", err
	}
	defer os.Remove(log.Name())
	defer log.Close()
	verified := false
	defer func() {
		if !verified {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = execSSHControl(cleanupCtx, socket, "exit")
			_ = os.Remove(socket)
		}
	}()
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		detail, _ := os.ReadFile(log.Name())
		return "", fmt.Errorf("establish SSH exec connection: %w: %s", err, strings.TrimSpace(string(detail)))
	}
	// Pod networking is shared across containers. Compare mount namespaces over
	// both channels before trusting that port 2222 belongs to the selected one.
	var api, remote bytes.Buffer
	probe := []string{"readlink", "/proc/self/ns/mnt"}
	if err := c.Client.ExecInteractiveInContainer(ctx, id.Namespace, id.Pod, id.Container, false, probe, nil, &api, io.Discard); err != nil {
		return "", fmt.Errorf("verify dev container: %w", err)
	}
	if err := execSSHRun(ctx, socket, probe, nil, &remote, io.Discard); err != nil {
		return "", err
	}
	if strings.TrimSpace(api.String()) == "" || api.String() != remote.String() {
		return "", errors.New("SSH server does not belong to the selected dev container")
	}
	pod, err := c.GetPodSummary(ctx, id.Namespace, id.Pod)
	if err != nil {
		return "", err
	}
	started, err := execSSHStarted(*pod, id.Container)
	if err != nil || pod.UID != id.UID || started != id.Started {
		return "", errors.New("dev container changed during SSH connection setup; command was not sent")
	}
	if err := os.WriteFile(metadata, data, 0600); err != nil {
		return "", err
	}
	verified = true
	return socket, nil
}

func pruneExecSSH(dir string) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sock.json"))
	for _, path := range paths {
		socket := strings.TrimSuffix(path, ".json")
		if _, err := os.Stat(socket); os.IsNotExist(err) {
			_ = os.Remove(path)
		}
	}
}

func stopExecSSHSession(session string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(home, ".okdev", "exec-ssh")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	dir, err := execSSHDirectory()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := execSSHLock(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sock.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var id execSSHIdentity
		if json.Unmarshal(data, &id) != nil || id.Session != session {
			continue
		}
		socket := strings.TrimSuffix(path, ".json")
		if err := execSSHControl(ctx, socket, "exit"); err != nil {
			if execSSHControl(ctx, socket, "check") == nil {
				return err
			}
		}
		_ = os.Remove(socket)
		_ = os.Remove(path)
	}
	pruneExecSSH(dir)
	return ctx.Err()
}

func (c *execSSHClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return c.ExecInteractiveInContainer(ctx, namespace, pod, c.container, tty, command, stdin, stdout, stderr)
}

func (c *execSSHClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	id, ok := c.identities[pod]
	if !ok || namespace != c.namespace || container != c.container || tty {
		return &connect.DeliveryError{Err: errors.New("SSH exec target is outside the validated selection")}
	}
	socket, err := c.master(ctx, id)
	if err != nil {
		return &connect.DeliveryError{Err: err}
	}
	return execSSHRun(ctx, socket, command, stdin, stdout, stderr)
}

// SSH uses status 255 for both remote exits and transport failure. A random
// completion footer preserves all remote exit codes without guessing from 255.
func execSSHRun(ctx context.Context, socket string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	marker := "\x00OKDEV_EXEC_" + hex.EncodeToString(random[:]) + ":"
	wrapper := "( " + shellJoinArgv(command) + " ); rc=$?; printf '\\000OKDEV_EXEC_" + hex.EncodeToString(random[:]) + ":%d\\000' \"$rc\" >&2; exit 0"
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	footer := &execSSHFooter{out: stderr, marker: marker}
	cmd := exec.CommandContext(ctx, "ssh", append(execSSHArgs(socket), "127.0.0.1", wrapper)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, footer
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	code, complete, writeErr := footer.finish()
	if ctx.Err() != nil {
		return &connect.DeliveryError{Err: ctx.Err()}
	}
	if err != nil {
		return &connect.DeliveryError{Err: err}
	}
	if writeErr != nil {
		return &connect.DeliveryError{Err: writeErr}
	}
	if !complete {
		return &connect.DeliveryError{Err: errors.New("missing SSH command completion record")}
	}
	if code != 0 {
		return k8sexec.CodeExitError{Err: fmt.Errorf("remote command exited with code %d", code), Code: code}
	}
	return nil
}

type execSSHFooter struct {
	out    io.Writer
	marker string
	tail   []byte
}

func (w *execSSHFooter) Write(p []byte) (int, error) {
	w.tail = append(w.tail, p...)
	keep := 0
	if i := bytes.LastIndex(w.tail, []byte(w.marker)); i >= 0 && len(w.tail)-i <= len(w.marker)+4 {
		keep = len(w.tail) - i
	} else {
		for n := min(len(w.tail), len(w.marker)-1); n > 0; n-- {
			if bytes.HasSuffix(w.tail, []byte(w.marker[:n])) {
				keep = n
				break
			}
		}
	}
	if n := len(w.tail) - keep; n > 0 {
		if _, err := w.out.Write(w.tail[:n]); err != nil {
			return 0, err
		}
		w.tail = append(w.tail[:0], w.tail[n:]...)
	}
	return len(p), nil
}
func (w *execSSHFooter) finish() (int, bool, error) {
	code, complete := 0, false
	if i := bytes.LastIndex(w.tail, []byte(w.marker)); i >= 0 && len(w.tail) > 0 && w.tail[len(w.tail)-1] == 0 {
		value := string(w.tail[i+len(w.marker) : len(w.tail)-1])
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 && parsed <= 255 && strconv.Itoa(parsed) == value {
			code, complete, w.tail = parsed, true, w.tail[:i]
		}
	}
	_, err := w.out.Write(w.tail)
	return code, complete, err
}

type execSSHForward struct{ *kube.Client }

func (f execSSHForward) PortForward(ctx context.Context, namespace, pod string, ports []string, out, errOut io.Writer) error {
	return f.PortForwardOnceOnAddresses(ctx, namespace, pod, []string{"127.0.0.1"}, ports, out, errOut)
}

func newExecSSHProxyCmd(opts *Options) *cobra.Command {
	var encoded string
	cmd := &cobra.Command{Use: "exec-ssh-proxy", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		var id execSSHIdentity
		if err := json.Unmarshal([]byte(encoded), &id); err != nil {
			return err
		}
		client := &kube.Client{Context: opts.Context}
		connection, err := client.ConnectionIdentity()
		if err != nil {
			return err
		}
		if connection != id.Connection {
			return errors.New("Kubernetes connection identity changed")
		}
		pod, err := client.GetPodSummary(cmd.Context(), id.Namespace, id.Pod)
		if err != nil {
			return err
		}
		started, err := execSSHStarted(*pod, id.Container)
		if err != nil {
			return err
		}
		if pod.UID != id.UID || started != id.Started {
			return errors.New("SSH target was replaced")
		}
		if err := checkSessionAccessPods(&Options{Owner: id.Owner}, id.Namespace, id.Session, true, []kube.PodSummary{*pod}); err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		local, err := reserveEphemeralPort()
		if err != nil {
			return err
		}
		stop, actualPort, err := startSSHPortForwardWithFallback(ctx, execSSHForward{client}, id.Namespace, id.Pod, local, sshPort)
		if err != nil {
			return err
		}
		defer stop()
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", actualPort))
		if err != nil {
			return err
		}
		defer conn.Close()
		done := make(chan error, 2)
		go func() { _, err := io.Copy(conn, cmd.InOrStdin()); done <- err }()
		go func() { _, err := io.Copy(cmd.OutOrStdout(), conn); done <- err }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	cmd.Flags().StringVar(&encoded, "identity", "", "Validated connection identity")
	return cmd
}
