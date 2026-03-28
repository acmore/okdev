package syncthing

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
)

const releaseBaseURL = "https://github.com/syncthing/syncthing/releases/download"
const installerHTTPTimeout = 60 * time.Second
const installerChecksumMaxBytes int64 = 1 << 20

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

var installerHTTPClient httpDoer = &http.Client{Timeout: installerHTTPTimeout}
var installerArchiveMaxBytes int64 = 512 << 20

func EnsureBinary(ctx context.Context, version string, autoInstall bool) (string, error) {
	if !autoInstall {
		if p, err := lookupInPath("syncthing"); err == nil {
			return p, nil
		}
		return "", errors.New("syncthing not found in PATH and autoInstall is disabled")
	}
	if version == "" {
		version = config.DefaultSyncthingVersion
	}

	platform, archiveNames, err := platformArchiveNames(version)
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

	checksumURL := fmt.Sprintf("%s/%s/sha256sum.txt.asc", releaseBaseURL, version)
	checksums, err := httpGet(ctx, checksumURL)
	if err != nil {
		return "", fmt.Errorf("download syncthing checksums: %w", err)
	}
	archiveName, err := selectArchiveName(string(checksums), archiveNames)
	if err != nil {
		return "", err
	}
	archiveURL := fmt.Sprintf("%s/%s/%s", releaseBaseURL, version, archiveName)
	want, err := checksumForArchive(string(checksums), archiveName)
	if err != nil {
		return "", err
	}
	archiveFile, got, err := downloadArchiveToTemp(ctx, archiveURL, archiveName)
	if err != nil {
		return "", fmt.Errorf("download syncthing archive: %w", err)
	}
	defer os.Remove(archiveFile)

	if !strings.EqualFold(want, got) {
		return "", fmt.Errorf("checksum mismatch for %s: want %s got %s", archiveName, want, got)
	}

	tmp := binPath + ".tmp"
	if err := extractSyncthingBinaryFromFileToPath(archiveFile, archiveName, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, binPath); err != nil {
		return "", err
	}
	return binPath, nil
}

func platformArchiveNames(version string) (platform string, archiveNames []string, err error) {
	osName := runtime.GOOS
	arch := runtime.GOARCH
	if osName != "linux" && osName != "darwin" {
		return "", nil, fmt.Errorf("unsupported OS %q for syncthing auto-install", osName)
	}
	if arch != "amd64" && arch != "arm64" {
		return "", nil, fmt.Errorf("unsupported architecture %q for syncthing auto-install", arch)
	}
	platform = osName + "-" + arch
	if osName == "linux" {
		return platform, []string{fmt.Sprintf("syncthing-linux-%s-%s.tar.gz", arch, version)}, nil
	}
	// macOS assets are distributed as zip archives under "macos-<arch>" names.
	return platform, []string{
		fmt.Sprintf("syncthing-macos-%s-%s.zip", arch, version),
		fmt.Sprintf("syncthing-darwin-%s-%s.tar.gz", arch, version),
	}, nil
}

func selectArchiveName(checksums string, archiveNames []string) (string, error) {
	for _, archiveName := range archiveNames {
		if _, err := checksumForArchive(checksums, archiveName); err == nil {
			return archiveName, nil
		}
	}
	return "", fmt.Errorf("checksum not found for candidate archives: %s", strings.Join(archiveNames, ", "))
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := installerHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, installerChecksumMaxBytes))
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

func extractSyncthingBinaryFromFileToPath(archivePath, archiveName, outPath string) error {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractSyncthingBinaryFromZipToPath(archivePath, outPath)
	}
	return extractSyncthingBinaryFromTarGzToPath(archivePath, outPath)
}

func extractSyncthingBinaryFromTarGzToPath(archivePath, outPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if !looksLikeSyncthingBinaryPath(hdr.Name, hdr.Size) {
			continue
		}
		if strings.HasSuffix(hdr.Name, "/syncthing") || hdr.Name == "syncthing" {
			out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				_ = os.Remove(outPath)
				return err
			}
			if err := out.Close(); err != nil {
				_ = os.Remove(outPath)
				return err
			}
			return nil
		}
	}
	return errors.New("syncthing binary not found in archive")
}

func extractSyncthingBinaryFromZipToPath(archivePath, outPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !looksLikeSyncthingBinaryPath(f.Name, int64(f.UncompressedSize64)) {
			continue
		}
		if strings.HasSuffix(f.Name, "/syncthing") || f.Name == "syncthing" {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
			if err != nil {
				_ = rc.Close()
				return err
			}
			if _, err := io.Copy(out, rc); err != nil {
				_ = rc.Close()
				_ = out.Close()
				_ = os.Remove(outPath)
				return err
			}
			if err := rc.Close(); err != nil {
				_ = out.Close()
				_ = os.Remove(outPath)
				return err
			}
			if err := out.Close(); err != nil {
				_ = os.Remove(outPath)
				return err
			}
			return nil
		}
	}
	return errors.New("syncthing binary not found in archive")
}

func looksLikeSyncthingBinaryPath(name string, size int64) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) == 0 || parts[len(parts)-1] != "syncthing" {
		return false
	}
	// Skip service/unit helper files also named "syncthing".
	for _, p := range parts {
		if p == "etc" {
			return false
		}
	}
	// Actual binaries are large; helper files are tiny text files.
	return size > 1<<20
}

func downloadArchiveToTemp(ctx context.Context, archiveURL, archiveName string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := installerHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("%s returned %s", archiveURL, resp.Status)
	}

	tmpFile, err := os.CreateTemp("", "okdev-syncthing-"+strings.ReplaceAll(archiveName, "/", "_")+"-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
	}()

	hasher := sha256.New()
	limitedBody := io.LimitReader(resp.Body, installerArchiveMaxBytes+1)
	written, err := io.Copy(io.MultiWriter(tmpFile, hasher), limitedBody)
	if err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	if written > installerArchiveMaxBytes {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("archive %s exceeds download limit of %d bytes", archiveName, installerArchiveMaxBytes)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", err
	}
	return tmpName, fmt.Sprintf("%x", hasher.Sum(nil)), nil
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
