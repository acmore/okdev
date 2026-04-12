package cli

import (
	"strings"
	"testing"
)

func TestParseCpArgs(t *testing.T) {
	tests := []struct {
		name       string
		src, dst   string
		wantLocal  string
		wantRemote string
		wantUpload bool
		wantErr    string
	}{
		{
			name: "upload", src: "./foo", dst: ":/bar",
			wantLocal: "./foo", wantRemote: "/bar", wantUpload: true,
		},
		{
			name: "download", src: ":/bar", dst: "./foo",
			wantLocal: "./foo", wantRemote: "/bar", wantUpload: false,
		},
		{
			name: "both local", src: "./a", dst: "./b",
			wantErr: "exactly one",
		},
		{
			name: "both remote", src: ":/a", dst: ":/b",
			wantErr: "exactly one",
		},
		{
			name: "colon only", src: ":", dst: "./foo",
			wantErr: "remote path cannot be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, remote, upload, err := parseCpArgs(tt.src, tt.dst)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if local != tt.wantLocal || remote != tt.wantRemote || upload != tt.wantUpload {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", local, remote, upload, tt.wantLocal, tt.wantRemote, tt.wantUpload)
			}
		})
	}
}

func TestValidateCpFlags(t *testing.T) {
	tests := []struct {
		name     string
		allPods  bool
		podNames []string
		role     string
		labels   []string
		exclude  []string
		wantErr  string
	}{
		{name: "all and role mutually exclusive", allPods: true, role: "worker", wantErr: "mutually exclusive"},
		{name: "exclude without selector", exclude: []string{"p1"}, wantErr: "--exclude requires"},
		{name: "exclude with pod", podNames: []string{"p1"}, exclude: []string{"p1"}, wantErr: "--exclude cannot be used with --pod"},
		{name: "valid all", allPods: true},
		{name: "valid role", role: "worker"},
		{name: "no flags", allPods: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCpFlags(tt.allPods, tt.podNames, tt.role, tt.labels, tt.exclude)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestMultiPodDownloadPath(t *testing.T) {
	if got := multiPodDownloadPath("/tmp/out", "worker-0", "/workspace/result.txt", false); got != "/tmp/out/worker-0/result.txt" {
		t.Fatalf("file download path = %q", got)
	}
	if got := multiPodDownloadPath("/tmp/out", "worker-0", "/workspace/results", true); got != "/tmp/out/worker-0" {
		t.Fatalf("directory download path = %q", got)
	}
}
