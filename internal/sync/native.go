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
	StreamFromPod(context.Context, string, string, string, io.Writer) error
	ExecSh(context.Context, string, string, string) ([]byte, error)
}

type Report struct {
	UploadBytes   int64
	DownloadBytes int64
	Paths         int
	SkippedPaths  int
}

type RunOptions struct {
	Force bool
}

type uploadFingerprintEntry struct {
	podInstance string
	value       string
}

var uploadFingerprintCache = struct {
	mu sync.Mutex
	m  map[string]uploadFingerprintEntry
}{
	m: map[string]uploadFingerprintEntry{},
}

var uploadPathLocks sync.Map

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
	return RunOnceWithOptions(parent, mode, k, namespace, pod, pairs, excludes, RunOptions{})
}

func RunOnceWithOptions(parent context.Context, mode string, k Client, namespace, pod string, pairs []Pair, excludes []string, opts RunOptions) (Report, error) {
	if parent == nil {
		parent = context.Background()
	}
	podInstance := resolvePodInstance(parent, k, namespace, pod)

	var report Report
	switch mode {
	case "up":
		for _, p := range pairs {
			ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
			stats, err := syncUpPath(ctx, k, namespace, pod, podInstance, p, excludes, opts.Force)
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
			stats, err := runOnceDirection(parent, direction, k, namespace, pod, podInstance, pairs, excludes, opts)
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

func runOnceDirection(parent context.Context, mode string, k Client, namespace, pod, podInstance string, pairs []Pair, excludes []string, opts RunOptions) (Report, error) {
	var report Report
	switch mode {
	case "up":
		for _, p := range pairs {
			ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
			stats, err := syncUpPath(ctx, k, namespace, pod, podInstance, p, excludes, opts.Force)
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
	default:
		return Report{}, fmt.Errorf("unsupported mode %q (supported: up|down)", mode)
	}
	return report, nil
}

type pathStats struct {
	UploadBytes   int64
	DownloadBytes int64
	Skipped       bool
}

func syncUpPath(ctx context.Context, k Client, namespace, pod, podInstance string, p Pair, excludes []string, force bool) (pathStats, error) {
	absLocal, err := filepath.Abs(p.Local)
	if err != nil {
		return pathStats{}, err
	}
	st, err := os.Stat(absLocal)
	if err != nil {
		return pathStats{}, err
	}

	cacheKey := uploadFingerprintCacheKey(namespace, pod, p, excludes)
	if !st.IsDir() {
		return withUploadKeyLock(cacheKey, func() (pathStats, error) {
			fingerprint := fmt.Sprintf("file:%d:%d", st.Size(), st.ModTime().UnixNano())
			if !force && uploadFingerprintMatches(cacheKey, podInstance, fingerprint) {
				return pathStats{Skipped: true}, nil
			}
			if err := k.CopyToPod(ctx, namespace, absLocal, pod, p.Remote); err != nil {
				return pathStats{}, err
			}
			setUploadFingerprint(cacheKey, podInstance, fingerprint)
			return pathStats{UploadBytes: st.Size()}, nil
		})
	}

	fingerprint, err := localDirFingerprint(absLocal, excludes)
	if err != nil {
		return pathStats{}, err
	}
	return withUploadKeyLock(cacheKey, func() (pathStats, error) {
		if !force && uploadFingerprintMatches(cacheKey, podInstance, fingerprint) {
			return pathStats{Skipped: true}, nil
		}

		stream, waitTar, err := startLocalTarStream(ctx, absLocal, excludes)
		if err != nil {
			return pathStats{}, err
		}
		defer stream.Close()

		countingStream := &countingReader{Reader: stream}
		if err := k.ExtractTarToPod(ctx, namespace, pod, p.Remote, countingStream); err != nil {
			if tarErr := waitTar(); tarErr != nil {
				return pathStats{}, tarErr
			}
			return pathStats{}, err
		}
		if err := waitTar(); err != nil {
			return pathStats{}, err
		}
		setUploadFingerprint(cacheKey, podInstance, fingerprint)
		return pathStats{UploadBytes: countingStream.BytesRead()}, nil
	})
}

func syncDownPath(ctx context.Context, k Client, namespace, pod string, p Pair, excludes []string) (pathStats, error) {
	absLocal, err := filepath.Abs(p.Local)
	if err != nil {
		return pathStats{}, err
	}
	if err := os.MkdirAll(absLocal, 0o755); err != nil {
		return pathStats{}, err
	}

	pr, pw := io.Pipe()
	extractErrCh := make(chan error, 1)
	go func() {
		extractErrCh <- extractTarStream(pr, absLocal)
		_ = pr.Close()
	}()

	countWriter := &countingWriteCloser{WriteCloser: pw}
	streamErr := k.StreamFromPod(ctx, namespace, pod, buildRemoteTarStreamCommand(p.Remote, excludes), countWriter)
	if streamErr != nil {
		_ = pw.CloseWithError(streamErr)
	} else {
		streamErr = pw.Close()
	}

	extractErr := <-extractErrCh
	if streamErr != nil {
		return pathStats{}, streamErr
	}
	if extractErr != nil {
		return pathStats{}, extractErr
	}
	return pathStats{DownloadBytes: countWriter.BytesWritten()}, nil
}

func buildRemoteTarStreamCommand(remoteDir string, excludes []string) string {
	args := []string{"tar", "-cf", "-"}
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

func startLocalTarStream(ctx context.Context, srcDir string, excludes []string) (io.ReadCloser, func() error, error) {
	args := []string{"-cf", "-"}
	for _, ex := range excludes {
		ex = strings.TrimSpace(ex)
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

func extractTarStream(stream io.Reader, destDir string) error {
	cmd := exec.Command("tar", "-xf", "-", "-C", destDir)
	cmd.Stdin = stream
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract tar stream: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func resolvePodInstance(parent context.Context, k Client, namespace, pod string) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := k.ExecSh(ctx, namespace, pod, "cat /proc/1/cpuset 2>/dev/null || hostname")
	if err != nil {
		return pod
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return pod
	}
	return id
}

type countingReader struct {
	io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	return n, err
}

func (r *countingReader) BytesRead() int64 {
	return r.n
}

type countingWriteCloser struct {
	io.WriteCloser
	n int64
}

func (w *countingWriteCloser) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *countingWriteCloser) BytesWritten() int64 {
	return w.n
}

func ShellEscape(s string) string {
	return shellutil.Quote(s)
}

func uploadFingerprintCacheKey(namespace, pod string, p Pair, excludes []string) string {
	return namespace + "|" + pod + "|" + p.Local + "|" + p.Remote + "|" + strings.Join(excludes, ";")
}

func uploadFingerprintMatches(key, podInstance, value string) bool {
	entry, ok := getUploadFingerprint(key)
	if !ok {
		return false
	}
	if entry.value != value {
		return false
	}
	if entry.podInstance == podInstance {
		return true
	}
	clearUploadFingerprint(key)
	return false
}

func getUploadFingerprint(key string) (uploadFingerprintEntry, bool) {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	v, ok := uploadFingerprintCache.m[key]
	return v, ok
}

func setUploadFingerprint(key, podInstance, value string) {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	uploadFingerprintCache.m[key] = uploadFingerprintEntry{podInstance: podInstance, value: value}
}

func clearUploadFingerprint(key string) {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	delete(uploadFingerprintCache.m, key)
}

func ResetUploadFingerprintCache() {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	uploadFingerprintCache.m = map[string]uploadFingerprintEntry{}
}

func uploadFingerprintCacheLen() int {
	uploadFingerprintCache.mu.Lock()
	defer uploadFingerprintCache.mu.Unlock()
	return len(uploadFingerprintCache.m)
}

func withUploadKeyLock(key string, fn func() (pathStats, error)) (pathStats, error) {
	v, _ := uploadPathLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
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
