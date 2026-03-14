package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	flag "github.com/spf13/pflag"
)

func main() {
	port := flag.Int("port", 2222, "SSH listen port")
	authorizedKeysPath := flag.String("authorized-keys", "/var/okdev/authorized_keys", "Path to authorized_keys file")
	shell := flag.String("shell", "", "Shell to use (auto-detect if empty)")
	flag.Parse()

	if *shell == "" {
		*shell = detectShell()
	}

	keys, err := loadAuthorizedKeys(*authorizedKeysPath)
	if err != nil {
		log.Fatalf("failed to load authorized keys: %v", err)
	}

	srv := &ssh.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: sessionHandler(*shell),
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": sftpHandler,
		},
	}

	if keys != nil {
		srv.PublicKeyHandler = func(ctx ssh.Context, key ssh.PublicKey) bool {
			for _, k := range keys {
				if ssh.KeysEqual(k, key) {
					return true
				}
			}
			return false
		}
	}

	log.Printf("okdev-sshd listening on :%d", *port)
	log.Fatal(srv.ListenAndServe())
}

func detectShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

func loadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var keys []ssh.PublicKey
	for len(data) > 0 {
		pubKey, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return nil, err
		}
		keys = append(keys, pubKey)
		data = rest
	}
	return keys, nil
}

func sessionHandler(shell string) ssh.Handler {
	return func(s ssh.Session) {
		cmd := buildCmd(s, shell)

		ptyReq, winCh, isPty := s.Pty()
		if isPty {
			if err := handlePTY(cmd, s, ptyReq, winCh); err != nil {
				exitCode := exitStatus(err)
				_ = s.Exit(exitCode)
				return
			}
			_ = s.Exit(0)
			return
		}

		if err := handleNoPTY(cmd, s); err != nil {
			exitCode := exitStatus(err)
			_ = s.Exit(exitCode)
			return
		}
		_ = s.Exit(0)
	}
}

func buildCmd(s ssh.Session, shell string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(s.RawCommand()) == 0 {
		cmd = exec.Command(shell, "-l")
	} else {
		cmd = exec.Command(shell, "-lc", s.RawCommand())
	}
	cmd.Env = append(os.Environ(), s.Environ()...)
	return cmd
}

func handlePTY(cmd *exec.Cmd, s ssh.Session, ptyReq ssh.Pty, winCh <-chan ssh.Window) error {
	if ptyReq.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
	}
	f, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer f.Close()

	go func() {
		for win := range winCh {
			setWinsize(f, win.Width, win.Height)
		}
	}()

	go io.Copy(f, s)

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(s, f)
	}()

	if err := cmd.Wait(); err != nil {
		return err
	}
	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
	return nil
}

func handleNoPTY(cmd *exec.Cmd, s ssh.Session) error {
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdin, _ := cmd.StdinPipe()

	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); io.Copy(stdin, s); stdin.Close() }()
	go func() { defer wg.Done(); io.Copy(s, stdout) }()
	go func() { defer wg.Done(); io.Copy(s.Stderr(), stderr) }()
	wg.Wait()

	return cmd.Wait()
}

func setWinsize(f *os.File, w, h int) {
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0})))
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return ws.ExitStatus()
		}
	}
	return 1
}

func sftpHandler(s ssh.Session) {
	server, err := sftp.NewServer(s)
	if err != nil {
		log.Printf("sftp server init error: %v", err)
		return
	}
	if err := server.Serve(); err == io.EOF {
		server.Close()
	}
}
