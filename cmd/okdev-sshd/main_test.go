package main

import (
	"testing"

	"github.com/gliderlabs/ssh"
)

func TestNewServerEnablesLocalPortForwarding(t *testing.T) {
	srv := newServer(":2222", "/bin/sh", nil)

	if srv.ChannelHandlers == nil {
		t.Fatal("expected channel handlers to be configured")
	}
	if _, ok := srv.ChannelHandlers["session"]; !ok {
		t.Fatal("expected default session channel handler")
	}
	if _, ok := srv.ChannelHandlers["direct-tcpip"]; !ok {
		t.Fatal("expected direct-tcpip channel handler for ssh forwarding")
	}
	if srv.LocalPortForwardingCallback == nil {
		t.Fatal("expected local port forwarding callback")
	}
	if !srv.LocalPortForwardingCallback(nil, "127.0.0.1", 8080) {
		t.Fatal("expected local port forwarding to be allowed")
	}
}

func TestNewServerAddsPublicKeyHandlerWhenKeysProvided(t *testing.T) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE9mN6e2Q2x8tQz4pT2r8j04YfGLwRoTSesFiNUFDXL9 test\n"))
	if err != nil {
		t.Fatalf("parse authorized key: %v", err)
	}

	srv := newServer(":2222", "/bin/sh", []ssh.PublicKey{pub})
	if srv.PublicKeyHandler == nil {
		t.Fatal("expected public key handler to be configured")
	}
}
