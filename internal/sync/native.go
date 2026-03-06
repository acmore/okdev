package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/shellutil"
)

type Pair struct {
	Local  string
	Remote string
}

type Client interface {
	CopyToPod(context.Context, string, string, string, string) error
	CopyFromPod(context.Context, string, string, string, string) error
	ExtractTarToPod(context.Context, string, string, string, io.Reader) error
	ExecSh(context.Context, string, string, string) ([]byte, error)
}

type Report struct {
	UploadBytes   int64
	DownloadBytes int64
	Paths         int
	SkippedPaths  int
}

var uploadFingerprintCache = struct {
	mu sync.Mutex
	m  map[string]string
}{
	m: map[string]string{},
}

func ParsePairs(configured []string, defaultRemote string) ([]Pair, error) {
	if len(configured) == 0 {
		return []Pair{{Local: ".", Remote: defaultRemote}}, nil
	}
	out := make([]Pair, 0, len(configured))
	for _, item := range configured {
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid sync path mapping %q, expected local:remote", item)
		}
		out = append(out, Pair{Local: strings.TrimSpace(parts[0]), Remote: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

func RunOnce(parent context.Context, mode string, k Client, namespace, pod string, pairs []Pair, excludes []string) error {
	_, err := RunOnceWithReport(parent, mode, k, namespace, pod, pairs, excludes)
	return err
}

func RunOnceWithReport(parent context.Context, mode string, k Client, namespace, pod string, pairs []Pair, excludes []string) (Report, error) {
	if parent == nil {
		parent = context.Background()
	}
	var report Report
	switch mode {
	case "up":
		for _, p := range pairs {
			ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
			stats, err := syncUpPath(ctx, k, namespace, pod, p, excludes)
			cancel()
			if err != nil {
				return Report{}, err
			}
			report.UploadBytes += stats.UploadBytes
			report.Paths++
			if stats.Skipped {
				report.SkippedPaths++
			}
		}
	case "down":
		for _, p := range pairs {
			ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
			stats, err := syncDownPath(ctx, k, namespace, pod, p, excludes)
			cancel()
			if err != nil {
				return Report{}, err
			}
			report.DownloadBytes += stats.DownloadBytes
			report.Paths++
			if stats.Skipped {
				report.SkippedPaths++
			}
		}
	case "bi":
		var mu sync.Mutex
		var wg sync.WaitGroup
		var firstErr error
		run := func(direction string) {
			defer wg.Done()
			stats, err := RunOnceWithReport(parent, direction, k, namespace, pod, pairs, excludes)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			report.UploadBytes += stats.UploadBytes
			report.DownloadBytes += stats.DownloadBytes
			if stats.Paths > report.Paths {
				report.Paths = stats.Paths
			}
			report.SkippedPaths += stats.SkippedPaths
		}
		wg.Add(2)
		go run("up")
		go run("down")
		wg.Wait()
		if firstErr != nil {
			return Report{}, firstErr
		}
	default:
		return Report{}, fmt.Errorf("unsupported mode %q (supported: up|down|bi)", mode)
	}
	return report, nil
}

type pathStats struct {
	UploadBytes   int64
	DownloadBytes int64
	Skipped       bool
}

func syncUpPath(ctx context.Context, k Client, namespace, pod string, p Pair, excludes []string) (pathStats, error) {
	absLocal, err := filepath.Abs(p.Local)
	if err != nil {
		return pathStats{}, err
	}
	st, err := os.Stat(absLocal)
	if err != nil {
		return pathStats{}, err
	}

	if !st.IsDir() {
		fingerprint := fmt.Sprintf("file:%d:%d", st.Size(), st.ModTime().UnixNano())
		cacheKey := uploadFingerprintCacheKey(namespace, pod, p, excludes)
		if cached, ok := getUploadFingerprint(cacheKey); ok && cached == fingerprint {
			return pathStats{Skipped: true}, nil
		}
		if err := k.CopyToPod(ctx, namespace, absLocal, pod, p.Remote); err != nil {
			return pathStats{}, err
		}
		setUploadFingerprint(cacheKey, fingerprint)
		return pathStats{UploadBytes: st.Size()}, nil
	}

	fingerprint, err := localDirFingerprint(absLocal, excludes)
	if err != nil {
		return pathStats{}, err
	}
	cacheKey := uploadFingerprintCacheKey(namespace, pod, p, excludes)
	if cached, ok := getUploadFingerprint(cacheKey); ok && cached == fingerprint {
		return pathStats{Skipped: true}, nil
	}

	stream, waitTar, err := startLocalTarStream(ctx, absLocal, excludes)
	if err != nil {
		return pathStats{}, err
	}
	if err := k.ExtractTarToPod(ctx, namespace, pod, p.Remote, stream); err != nil {
		_ = stream.Close()
		if tarErr := waitTar(); tarErr != nil {
			return pathStats{}, tarErr
		}
		return pathStats{}, err
	}
	if err := waitTar(); err != nil {
		return pathStats{}, err
	}
	setUploadFingerprint(cacheKey, fingerprint)
	return pathStats{}, nil
}

func syncDownPath(ctx context.Context, k Client, namespace, pod string, p Pair, excludes []string) (pathStats, error) {
	absLocal, err := filepath.Abs(p.Local)
	if err != nil {
		return pathStats{}, err
	}
	if err := os.MkdirAll(absLocal, 0o755); err != nil {
		return pathStats{}, err
	}

	remoteTar := "/tmp/okdev-sync-down.tar"
	localTar := filepath.Join(os.TempDir(), fmt.Sprintf("okdev-down-%d.tar", time.Now().UnixNano()))
	defer os.Remove(localTar)

	if _, err := k.ExecSh(ctx, namespace, pod, buildRemoteTarCommand(remoteTar, p.Remote, excludes)); err != nil {
		return pathStats{}, err
	}
	if err := k.CopyFromPod(ctx, namespace, pod, remoteTar, localTar); err != nil {
		return pathStats{}, err
	}
	if _, err := k.ExecSh(ctx, namespace, pod, fmt.Sprintf("rm -f %s", remoteTar)); err != nil {
		return pathStats{}, err
	}
	tarStat, err := os.Stat(localTar)
	if err != nil {
		return pathStats{}, err
	}
	if err := extractTar(localTar, absLocal); err != nil {
		return pathStats{}, err
	}
	return pathStats{DownloadBytes: tarStat.Size()}, nil
}

func buildRemoteTarCommand(remoteTar, remoteDir string, excludes []string) string {
	args := []string{"tar", "-cf", remoteTar}
	for _, ex := range excludes {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-C", remoteDir, ".")

	escaped := make([]string, 0, len(args))
	for _, a := range args {
		escaped = append(escaped, ShellEscape(a))
	}
	return strings.Join(escaped, " ")
}

func createTar(srcDir, outTar string, excludes []string) error {
	args := []string{"-cf", outTar}
	for _, ex := range excludes {
		ex := strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-C", srcDir, ".")
	cmd := exec.Command("tar", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create tar: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startLocalTarStream(ctx context.Context, srcDir string, excludes []string) (io.ReadCloser, func() error, error) {
	args := []string{"-cf", "-"}
	for _, ex := range excludes {
		ex := strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-C", srcDir, ".")

	pr, pw := io.Pipe()
	var errBuf strings.Builder
	cmd := exec.CommandContext(ctx, "tar", args...)
	cmd.Stdout = pw
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, nil, fmt.Errorf("start tar stream: %w", err)
	}
	errCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			wrapped := fmt.Errorf("create tar stream: %w (%s)", err, strings.TrimSpace(errBuf.String()))
			_ = pw.CloseWithError(wrapped)
			errCh <- wrapped
			return
		}
		_ = pw.Close()
		errCh <- nil
	}()
	return pr, func() error {
		return <-errCh
	}, nil
}

func extractTar(tarFile, destDir string) error {
	cmd := exec.Command("tar", "-xf", tarFile, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract tar: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ShellEscape(s string) string {
	return shellutil.Quote(s)
}

func uploadFingerprintCacheKey(namespace, pod string, p Pair, excludes []string) string {
	return namespace + "|" + pod + "|" + p.Local + "|" + p.Remote + "|" + strings.Join(excludes, ";")
}

func getUploadFingerprint(key string) (string, bool) {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	v, ok := uploadFingerprintCache.m[key]
	return v, ok
}

func setUploadFingerprint(key, value string) {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	uploadFingerprintCache.m[key] = value
}

func localDirFingerprint(root string, excludes []string) (string, error) {
	var files int64
	var bytes int64
	var latest int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if matchesExclude(rel, excludes, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		mod := info.ModTime().UnixNano()
		if mod > latest {
			latest = mod
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("dir:%d:%d:%d", files, bytes, latest), nil
}

func matchesExclude(rel string, excludes []string, isDir bool) bool {
	for _, ex := range excludes {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		dirPattern := strings.TrimSuffix(ex, "/")
		if strings.HasSuffix(ex, "/") {
			if rel == dirPattern || strings.HasPrefix(rel, dirPattern+"/") {
				return true
			}
		}
		if matched, _ := filepath.Match(ex, rel); matched {
			return true
		}
		if isDir {
			if matched, _ := filepath.Match(ex, rel+"/"); matched {
				return true
			}
		}
	}
	return false
}
