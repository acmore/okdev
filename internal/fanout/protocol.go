// Package fanout defines the wire protocol between the okdev CLI and the
// `okdev-sshd fanout` gateway driver. The CLI uploads nothing and parses
// nothing shell-generated: both ends are compiled from this package, so the
// only version skew possible is a stale sidecar image, which the CLI detects
// by the absence of a hello frame and handles by falling back to direct
// per-pod exec.
package fanout

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ProtocolVersion identifies the frame protocol. Bump when frames change
// incompatibly; the version is embedded in the frame prefixes so a newer CLI
// simply sees no hello from an older driver and falls back.
const ProtocolVersion = 1

const (
	helloPrefix = "__OKDEV_HELLO1__ "
	framePrefix = "__OKDEV_F1__ "
	donePrefix  = "__OKDEV_DONE1__ "
)

// Per-pod result statuses. `responded` covers any run where the remote
// command started and reported an exit status (including non-zero: the exit
// code is data). The other statuses mean the command's outcome is unknown or
// it never ran.
const (
	StatusResponded   = "responded"
	StatusUnreachable = "unreachable"
	StatusTimeout     = "timeout"
	StatusError       = "error"
	StatusMissing     = "missing"
)

// Target is one pod the driver must reach over the pod network.
type Target struct {
	Pod  string `json:"pod"`
	Addr string `json:"addr"`
}

// Request is what the CLI writes to the driver's stdin as a single JSON
// document. Exactly one of Command or Script must be set.
type Request struct {
	Version int      `json:"version"`
	User    string   `json:"user"`
	KeyPath string   `json:"keyPath"`
	Port    int      `json:"port"`
	Targets []Target `json:"targets"`
	// Command is a complete shell command string executed via the remote
	// pod's okdev-sshd (which runs it with `shell -lc`).
	Command string `json:"command,omitempty"`
	// Script is executed on each pod by writing it to a temp file (so
	// shebangs are honored) and running it with ScriptArgs.
	Script     []byte   `json:"script,omitempty"`
	ScriptArgs []string `json:"scriptArgs,omitempty"`
	// TimeoutSec bounds each pod's run; 0 means no per-pod timeout.
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// Fanout caps concurrent pod sessions; <=0 uses the driver default.
	Fanout int `json:"fanout,omitempty"`
	// Retries is the number of additional connect attempts after a dial
	// failure. Only dial failures are retried: once a session is
	// established the command may have run, so post-connect failures are
	// reported, never re-executed.
	Retries int `json:"retries,omitempty"`
}

// Result is one pod's outcome, emitted as a frame as soon as the pod
// completes.
type Result struct {
	Pod      string `json:"pod"`
	Status   string `json:"status"`
	Exit     int    `json:"exit"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// Hello is the first frame the driver emits. Its presence tells the CLI the
// gateway driver is live and speaking this protocol version.
type Hello struct {
	Version int `json:"version"`
}

// Done is the final frame; Count lets the CLI detect a truncated stream.
type Done struct {
	Count int `json:"count"`
}

func (r *Request) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("unsupported fanout protocol version %d", r.Version)
	}
	if len(r.Targets) == 0 {
		return fmt.Errorf("no fanout targets")
	}
	for _, t := range r.Targets {
		if strings.TrimSpace(t.Pod) == "" || strings.TrimSpace(t.Addr) == "" {
			return fmt.Errorf("fanout target missing pod or addr")
		}
	}
	hasCommand := strings.TrimSpace(r.Command) != ""
	hasScript := len(r.Script) > 0
	if hasCommand == hasScript {
		return fmt.Errorf("exactly one of command or script must be set")
	}
	if strings.TrimSpace(r.User) == "" {
		return fmt.Errorf("fanout request missing user")
	}
	if strings.TrimSpace(r.KeyPath) == "" {
		return fmt.Errorf("fanout request missing key path")
	}
	if r.Port <= 0 {
		return fmt.Errorf("fanout request missing port")
	}
	return nil
}

// encodeFrame renders prefix + base64(JSON(v)) + "\n". Base64 keeps a frame
// on a single line regardless of payload bytes, so frames survive any
// line-oriented transport untouched.
func encodeFrame(prefix string, v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return prefix + base64.StdEncoding.EncodeToString(data) + "\n", nil
}

func decodeFrame(line, prefix string, v any) (bool, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
	if !ok {
		return false, nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil {
		return true, fmt.Errorf("decode fanout frame: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return true, fmt.Errorf("parse fanout frame: %w", err)
	}
	return true, nil
}

func EncodeHello() (string, error) {
	return encodeFrame(helloPrefix, Hello{Version: ProtocolVersion})
}

func EncodeResult(r Result) (string, error) {
	return encodeFrame(framePrefix, r)
}

func EncodeDone(count int) (string, error) {
	return encodeFrame(donePrefix, Done{Count: count})
}

// Stream is the CLI-side view of a parsed driver stdout.
type Stream struct {
	HelloSeen bool
	DoneSeen  bool
	DoneCount int
	Results   []Result
}

// ParseStream scans the driver's stdout. Non-frame lines are ignored so
// stray output (login-shell noise on the gateway) can never corrupt results.
func ParseStream(r io.Reader) (*Stream, error) {
	s := &Stream{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var hello Hello
		if ok, err := decodeFrame(line, helloPrefix, &hello); ok {
			if err != nil {
				return nil, err
			}
			s.HelloSeen = true
			continue
		}
		var res Result
		if ok, err := decodeFrame(line, framePrefix, &res); ok {
			if err != nil {
				return nil, err
			}
			s.Results = append(s.Results, res)
			continue
		}
		var done Done
		if ok, err := decodeFrame(line, donePrefix, &done); ok {
			if err != nil {
				return nil, err
			}
			s.DoneSeen = true
			s.DoneCount = done.Count
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan fanout stream: %w", err)
	}
	return s, nil
}
