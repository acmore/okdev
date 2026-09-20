package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPortForwardCancellationStopsBlockedUpgrade(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("SPDY_fallback=%t", fallback), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fallback && r.Method == http.MethodGet {
					http.Error(w, "upgrade unavailable", http.StatusBadRequest)
					return
				}
				once.Do(func() { close(started) })
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			t.Setenv("KUBECONFIG", writeTLSTestKubeconfig(t, server))
			client := &Client{Context: "dev"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- client.PortForwardOnceOnAddresses(ctx, "demo", "pod", []string{"127.0.0.1"}, []string{"18080:8080"}, io.Discard, io.Discard)
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("upgrade exited before starting: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("upgrade request did not start")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled upgrade succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled upgrade did not return")
			}
		})
	}
}

func TestPortForwardSPDYFallbackTransfersData(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "use SPDY", http.StatusBadRequest)
			return
		}
		if _, err := httpstream.Handshake(r, w, []string{"portforward.k8s.io"}); err != nil {
			return
		}
		conn := httpstreamspdy.NewResponseUpgrader().UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
			if stream.Headers().Get("streamType") == "data" {
				go func() {
					<-replySent
					_, _ = io.Copy(stream, stream)
				}()
			}
			return nil
		})
		if conn != nil {
			defer conn.Close()
			<-conn.CloseChan()
		}
	}))
	defer server.Close()
	t.Setenv("KUBECONFIG", writeTLSTestKubeconfig(t, server))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- (&Client{Context: "dev"}).PortForwardOnceOnAddresses(ctx, "demo", "pod", []string{"127.0.0.1"}, []string{"0:8080"}, forwardReadyWriter{ready}, io.Discard)
	}()
	var address string
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatalf("forward exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not become ready")
	}
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("SPDY forwarding still works")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, received); err != nil || !bytes.Equal(payload, received) {
		t.Fatalf("forwarded data=%q err=%v", received, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("forward did not stop after cancellation")
	}
}

type forwardReadyWriter struct{ ready chan<- string }

func (w forwardReadyWriter) Write(p []byte) (int, error) {
	var address string
	if _, err := fmt.Sscanf(string(p), "Forwarding from %s ->", &address); err == nil {
		w.ready <- address
	}
	return len(p), nil
}
