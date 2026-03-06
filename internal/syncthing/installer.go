package syncthing

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releaseBaseURL = "https://github.com/syncthing/syncthing/releases/download"

func EnsureBinary(ctx context.Context, version string, autoInstall bool) (string, error) {
	if p, err := lookupInPath("syncthing"); err == nil {
		return p, nil
	}
	if !autoInstall {
		return "", errors.New("syncthing not found in PATH and autoInstall is disabled")
	}
	if version == "" {
		version = "v1.29.7"
	}

	platform, archiveName, err := platformArchiveName(version)
	if err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	binPath := filepath.Join(home, ".okdev", "bin", "syncthing", version, platform, "syncthing")
	if st, err := os.Stat(binPath); err == nil && !st.IsDir() {
		return binPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return "", err
	}

	archiveURL := fmt.Sprintf("%s/%s/%s", releaseBaseURL, version, archiveName)
	checksumURL := fmt.Sprintf("%s/%s/sha256sum.txt", releaseBaseURL, version)

	archive, err := httpGet(ctx, archiveURL)
	if err != nil {
		return "", fmt.Errorf("download syncthing archive: %w", err)
	}
	checksums, err := httpGet(ctx, checksumURL)
	if err != nil {
		return "", fmt.Errorf("download syncthing checksums: %w", err)
	}
	want, err := checksumForArchive(string(checksums), archiveName)
	if err != nil {
		return "", err
	}
	got := sha256Hex(archive)
	if !strings.EqualFold(want, got) {
		return "", fmt.Errorf("checksum mismatch for %s: want %s got %s", archiveName, want, got)
	}

	binary, err := extractSyncthingBinary(archive)
	if err != nil {
		return "", err
	}

	tmp := binPath + ".tmp"
	if err := os.WriteFile(tmp, binary, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, binPath); err != nil {
		return "", err
	}
	return binPath, nil
}

func platformArchiveName(version string) (platform string, archiveName string, err error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	if osName != "linux" && osName != "darwin" {
		return "", "", fmt.Errorf("unsupported OS %q for syncthing auto-install", osName)
	}
	if arch != "amd64" && arch != "arm64" {
		return "", "", fmt.Errorf("unsupported architecture %q for syncthing auto-install", arch)
	}
	platform = osName + "-" + arch
	archiveName = fmt.Sprintf("syncthing-%s-%s-%s.tar.gz", osName, arch, version)
	return platform, archiveName, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func checksumForArchive(checksums string, archiveName string) (string, error) {
	for _, line := range strings.Split(checksums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == archiveName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum for %s not found", archiveName)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func extractSyncthingBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if strings.HasSuffix(hdr.Name, "/syncthing") || hdr.Name == "syncthing" {
			return io.ReadAll(tr)
		}
	}
	return nil, errors.New("syncthing binary not found in archive")
}

func lookupInPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}
