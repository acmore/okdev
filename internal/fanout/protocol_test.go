package fanout

import (
	"strings"
	"testing"
)

func validRequest() Request {
	return Request{
		Version: ProtocolVersion,
		User:    "root",
		KeyPath: "~/.ssh/key",
		Port:    2222,
		Targets: []Target{{Pod: "a", Addr: "10.0.0.1"}},
		Command: "true",
	}
}

func TestRequestValidate(t *testing.T) {
	base := validRequest()
	if err := base.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"wrong version", func(r *Request) { r.Version = 99 }},
		{"no targets", func(r *Request) { r.Targets = nil }},
		{"target missing addr", func(r *Request) { r.Targets = []Target{{Pod: "a"}} }},
		{"neither command nor script", func(r *Request) { r.Command = "" }},
		{"both command and script", func(r *Request) { r.Script = []byte("x") }},
		{"missing user", func(r *Request) { r.User = "" }},
		{"missing key", func(r *Request) { r.KeyPath = " " }},
		{"missing port", func(r *Request) { r.Port = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.mutate(&req)
			if err := req.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestStreamRoundTrip(t *testing.T) {
	hello, err := EncodeHello()
	if err != nil {
		t.Fatal(err)
	}
	frame1, err := EncodeResult(Result{
		Pod:    "pod-a",
		Status: StatusResponded,
		Exit:   3,
		Stdout: []byte("line1\nline2\n\xff\xfebinary"),
		Stderr: []byte("warn"),
	})
	if err != nil {
		t.Fatal(err)
	}
	frame2, err := EncodeResult(Result{Pod: "pod-b", Status: StatusUnreachable, Exit: -1, Error: "dial tcp: refused", Attempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	done, err := EncodeDone(2)
	if err != nil {
		t.Fatal(err)
	}

	// Interleave driver noise that must be ignored, including a line that
	// merely resembles a frame.
	raw := "spurious login banner\n" + hello + frame1 + "__OKDEV_F1__ %%%not-base64%%%ignored-if-strict?\n" + frame2 + done

	stream, err := ParseStream(strings.NewReader(raw))
	if err == nil {
		t.Fatalf("expected error for corrupt frame line")
	}

	// Without the corrupt line, everything round-trips.
	raw = "spurious login banner\n" + hello + frame1 + frame2 + done
	stream, err = ParseStream(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.HelloSeen || !stream.DoneSeen || stream.DoneCount != 2 {
		t.Fatalf("unexpected stream meta: %+v", stream)
	}
	if len(stream.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(stream.Results))
	}
	a := stream.Results[0]
	if a.Pod != "pod-a" || a.Exit != 3 || string(a.Stdout) != "line1\nline2\n\xff\xfebinary" || string(a.Stderr) != "warn" {
		t.Fatalf("frame1 corrupted: %+v", a)
	}
	b := stream.Results[1]
	if b.Pod != "pod-b" || b.Status != StatusUnreachable || b.Attempts != 3 {
		t.Fatalf("frame2 corrupted: %+v", b)
	}
}

func TestParseStreamWithoutHello(t *testing.T) {
	stream, err := ParseStream(strings.NewReader("sh: /var/okdev/okdev-sshd: not found\n"))
	if err != nil {
		t.Fatal(err)
	}
	if stream.HelloSeen || len(stream.Results) != 0 {
		t.Fatalf("expected empty stream, got %+v", stream)
	}
}
