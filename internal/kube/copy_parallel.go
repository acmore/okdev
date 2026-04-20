package kube

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/acmore/okdev/internal/shellutil"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// RemoteFileEntry describes one regular file inside a remote directory tree.
// Path is relative to the enumerated root (no leading "./").
type RemoteFileEntry struct {
	Path string
	Size int64
}

// ListFilesUnder runs `find` + `stat` on the pod to enumerate regular files
// under remoteDir with their sizes. Paths returned are relative to remoteDir.
// The helper gracefully returns an empty list if the path is a regular file
// (callers should check beforehand via IsRemoteDir).
func (c *Client) ListFilesUnder(ctx context.Context, namespace, pod, container, remoteDir string) ([]RemoteFileEntry, error) {
	// Use -printf on GNU find when available (fast single-process output), falling
	// back to -exec stat for busybox. Emitting a leading SIZE\tPATH on each line
	// keeps parsing trivial and avoids shell-escaping surprises.
	script := fmt.Sprintf(
		"cd %s && (find . -type f -printf '%%s\\t%%P\\n' 2>/dev/null || find . -type f -exec stat -c '%%s\\t%%n' {} +)",
		shellutil.Quote(remoteDir),
	)
	out, err := c.ExecShInContainer(ctx, namespace, pod, container, script)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", remoteDir, err)
	}
	return parseListFilesOutput(out), nil
}

// parseListFilesOutput parses the SIZE\tPATH lines emitted by ListFilesUnder.
// Unparseable lines are skipped. Empty paths are dropped. Leading "./" is
// stripped so paths are always relative to the enumerated root.
func parseListFilesOutput(raw []byte) []RemoteFileEntry {
	var out []RemoteFileEntry
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		tab := strings.IndexByte(line, '\t')
		if tab <= 0 {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line[:tab]), 10, 64)
		if err != nil || size < 0 {
			continue
		}
		path := strings.TrimPrefix(line[tab+1:], "./")
		path = strings.TrimSpace(path)
		if path == "" || path == "." {
			continue
		}
		out = append(out, RemoteFileEntry{Path: path, Size: size})
	}
	return out
}

// bucketFilesByLPT distributes entries into n buckets using the
// longest-processing-time-first heuristic so each bucket receives a roughly
// equal total byte load. Input order is preserved only insofar as LPT permits.
// Returns at most n buckets; empty buckets are omitted.
func bucketFilesByLPT(entries []RemoteFileEntry, n int) [][]RemoteFileEntry {
	if n < 1 {
		n = 1
	}
	if len(entries) == 0 {
		return nil
	}
	if n >= len(entries) {
		out := make([][]RemoteFileEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, []RemoteFileEntry{e})
		}
		return out
	}

	sorted := make([]RemoteFileEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Size != sorted[j].Size {
			return sorted[i].Size > sorted[j].Size
		}
		return sorted[i].Path < sorted[j].Path
	})

	buckets := make([][]RemoteFileEntry, n)
	loads := make([]int64, n)
	for _, e := range sorted {
		idx := 0
		min := loads[0]
		for i := 1; i < n; i++ {
			if loads[i] < min {
				idx = i
				min = loads[i]
			}
		}
		buckets[idx] = append(buckets[idx], e)
		loads[idx] += e.Size
	}

	out := buckets[:0]
	for _, b := range buckets {
		if len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// CopyDirFromPodParallelWithOptions downloads remoteDir into localDir using
// up to parallel concurrent exec streams. Each worker owns a disjoint subset
// of files balanced by byte size. When parallel <= 1, or the remote contains
// zero files, this falls back to the single-stream implementation.
func (c *Client) CopyDirFromPodParallelWithOptions(ctx context.Context, namespace, pod, container, remoteDir, localDir string, parallel int, opts CopyOptions) error {
	if parallel <= 1 {
		return c.CopyDirFromPodWithOptions(ctx, namespace, pod, container, remoteDir, localDir, opts)
	}

	opts.Progress.phase("listing remote files")
	entries, err := c.ListFilesUnder(ctx, namespace, pod, container, remoteDir)
	if err != nil {
		opts.Progress.phase("")
		return err
	}
	opts.Progress.phase("")

	if len(entries) == 0 {
		return c.CopyDirFromPodWithOptions(ctx, namespace, pod, container, remoteDir, localDir, opts)
	}

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}

	buckets := bucketFilesByLPT(entries, parallel)
	return c.runParallelDirFromPod(ctx, namespace, pod, container, remoteDir, localDir, buckets, opts.Progress)
}

func (c *Client) runParallelDirFromPod(ctx context.Context, namespace, pod, container, remoteDir, localDir string, buckets [][]RemoteFileEntry, progress *CopyProgress) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(buckets))

	for _, bucket := range buckets {
		wg.Add(1)
		go func(files []RemoteFileEntry) {
			defer wg.Done()
			if err := c.runBucketDownload(ctx, cs, cfg, namespace, pod, container, remoteDir, localDir, files, progress); err != nil {
				errCh <- err
				cancel()
			}
		}(bucket)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			return e
		}
	}
	return nil
}

func (c *Client) runBucketDownload(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container, remoteDir, localDir string, files []RemoteFileEntry, progress *CopyProgress) error {
	if len(files) == 0 {
		return nil
	}

	args := make([]string, 0, len(files)+3)
	args = append(args, "tar", "cf", "-", "-C", shellutil.Quote(remoteDir))
	for _, f := range files {
		args = append(args, shellutil.Quote(f.Path))
	}
	script := strings.Join(args, " ")
	cmd := []string{"sh", "-lc", script}

	pr, pw := io.Pipe()
	var errBuf bytes.Buffer
	execErrCh := make(chan error, 1)
	go func() {
		err := c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, nil, pw, &errBuf, false)
		_ = pw.CloseWithError(err)
		execErrCh <- err
	}()

	_, extractErr := extractTarToDir(pr, localDir, progress)
	_ = pr.Close()
	execErr := <-execErrCh
	if extractErr != nil {
		return extractErr
	}
	if execErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", execErr, msg)
		}
		return execErr
	}
	return nil
}

// CopyDirToPodParallelWithOptions uploads localDir to remoteDir using up to
// parallel concurrent exec streams. It enumerates regular files under
// localDir, buckets them by byte size, and streams a separate tar per worker
// to `tar xf -` inside the pod.
func (c *Client) CopyDirToPodParallelWithOptions(ctx context.Context, namespace, pod, container, localDir, remoteDir string, parallel int, opts CopyOptions) error {
	if parallel <= 1 {
		return c.CopyDirToPodWithOptions(ctx, namespace, pod, container, localDir, remoteDir, opts)
	}

	opts.Progress.phase("scanning local files")
	entries, err := listLocalFiles(localDir)
	opts.Progress.phase("")
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return c.CopyDirToPodWithOptions(ctx, namespace, pod, container, localDir, remoteDir, opts)
	}

	buckets := bucketFilesByLPT(entries, parallel)
	return c.runParallelDirToPod(ctx, namespace, pod, container, localDir, remoteDir, buckets, opts.Progress)
}

// listLocalFiles enumerates regular files under dir with sizes, paths relative
// to dir. Symlinks and non-regular files are skipped so the resulting byte
// totals reflect what the tar walk will actually ship.
func listLocalFiles(dir string) ([]RemoteFileEntry, error) {
	var out []RemoteFileEntry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out = append(out, RemoteFileEntry{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) runParallelDirToPod(ctx context.Context, namespace, pod, container, localDir, remoteDir string, buckets [][]RemoteFileEntry, progress *CopyProgress) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// All workers share the same remote extraction target; a single mkdir -p
	// up front avoids racing N "mkdir -p" invocations on the pod.
	mkCmd := []string{"sh", "-lc", fmt.Sprintf("mkdir -p %s", shellutil.Quote(remoteDir))}
	if _, err := c.execCapture(ctx, namespace, pod, container, mkCmd); err != nil {
		return fmt.Errorf("prepare remote dir: %w", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(buckets))

	for _, bucket := range buckets {
		wg.Add(1)
		go func(files []RemoteFileEntry) {
			defer wg.Done()
			if err := c.runBucketUpload(ctx, cs, cfg, namespace, pod, container, localDir, remoteDir, files, progress); err != nil {
				errCh <- err
				cancel()
			}
		}(bucket)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		if e != nil {
			return e
		}
	}
	return nil
}

func (c *Client) runBucketUpload(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container, localDir, remoteDir string, files []RemoteFileEntry, progress *CopyProgress) error {
	if len(files) == 0 {
		return nil
	}

	pr, pw := io.Pipe()
	tarErrCh := make(chan error, 1)
	go func() {
		tarErrCh <- writeFilesTar(localDir, files, pw, progress)
		_ = pw.Close()
	}()

	cmd := []string{"sh", "-lc", fmt.Sprintf("tar xf - -C %s", shellutil.Quote(remoteDir))}
	var errBuf bytes.Buffer
	if err := c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, pr, io.Discard, &errBuf, false); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return <-tarErrCh
}

// writeFilesTar writes a tar archive containing only the listed regular files
// (paths relative to dir) to w, reporting progress per file. It does not emit
// directory entries; `tar xf -` on the receiving side creates parents via the
// implicit path components of each file entry.
func writeFilesTar(dir string, files []RemoteFileEntry, w io.Writer, progress *CopyProgress) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	for _, entry := range files {
		full := filepath.Join(dir, filepath.FromSlash(entry.Path))
		info, err := os.Stat(full)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = entry.Path
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		progress.fileStart(entry.Path, info.Size())
		var src io.Reader = f
		if progress != nil {
			src = &countingReader{r: f, progress: progress}
		}
		if _, err := io.Copy(tw, src); err != nil {
			_ = f.Close()
			return err
		}
		progress.fileEnd(entry.Path)
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
