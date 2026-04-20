package kube

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
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

// minFilesForParallel is the tree-size floor below which parallelism costs
// more (extra round-trip for enumeration, per-file stat overhead in tar)
// than it saves. Callers that request --parallel >1 on a tiny tree are
// silently downgraded to the single-stream recursive path.
const minFilesForParallel = 8

// CopyDirFromPodParallelWithOptions downloads remoteDir into localDir using
// up to parallel concurrent exec streams. Each worker owns a disjoint subset
// of files balanced by byte size. When parallel <= 1, or the remote contains
// fewer than minFilesForParallel files, this falls back to the single-stream
// implementation which runs one recursive `tar cf - <dir>` and avoids the
// per-file stat/open overhead that the parallel path incurs.
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

	if len(entries) < minFilesForParallel {
		return c.CopyDirFromPodWithOptions(ctx, namespace, pod, container, remoteDir, localDir, opts)
	}

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}

	buckets := bucketFilesByLPT(entries, parallel)
	return c.runParallelDirFromPod(ctx, namespace, pod, container, remoteDir, localDir, buckets, opts.Progress, opts.Compress)
}

func (c *Client) runParallelDirFromPod(ctx context.Context, namespace, pod, container, remoteDir, localDir string, buckets [][]RemoteFileEntry, progress *CopyProgress, compress bool) error {
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
			if err := c.runBucketDownload(ctx, cs, cfg, namespace, pod, container, remoteDir, localDir, files, progress, compress); err != nil {
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

func (c *Client) runBucketDownload(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container, remoteDir, localDir string, files []RemoteFileEntry, progress *CopyProgress, compress bool) error {
	if len(files) == 0 {
		return nil
	}

	// Feed the file list via stdin (`tar cf - -T -`) instead of packing every
	// path onto the command line. The arg-list approach trips ARG_MAX on
	// directories with tens of thousands of files; stdin scales indefinitely.
	// Paths containing newline characters would be misparsed here, but that's
	// rare enough to punt on — users hitting it can fall back to --parallel 1.
	var fileList bytes.Buffer
	fileList.Grow(64 * len(files))
	for _, f := range files {
		fileList.WriteString(f.Path)
		fileList.WriteByte('\n')
	}

	tarFlag := "cf"
	if compress {
		tarFlag = "czf"
	}
	script := fmt.Sprintf("tar %s - -C %s -T -", tarFlag, shellutil.Quote(remoteDir))
	cmd := []string{"sh", "-lc", script}

	pr, pw := io.Pipe()
	var errBuf bytes.Buffer
	execErrCh := make(chan error, 1)
	go func() {
		err := c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, &fileList, pw, &errBuf, false)
		_ = pw.CloseWithError(err)
		execErrCh <- err
	}()

	var tarSource io.Reader = pr
	var gzReader *gzip.Reader
	if compress {
		gz, gzErr := gzip.NewReader(pr)
		if gzErr != nil {
			_ = pr.Close()
			<-execErrCh
			return fmt.Errorf("decompress remote tar: %w", gzErr)
		}
		gzReader = gz
		tarSource = gz
	}
	_, extractErr := extractTarToDir(tarSource, localDir, progress)
	if gzReader != nil {
		_ = gzReader.Close()
	}
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
	if len(entries) < minFilesForParallel {
		return c.CopyDirToPodWithOptions(ctx, namespace, pod, container, localDir, remoteDir, opts)
	}

	buckets := bucketFilesByLPT(entries, parallel)
	return c.runParallelDirToPod(ctx, namespace, pod, container, localDir, remoteDir, buckets, opts.Progress, opts.Compress)
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

func (c *Client) runParallelDirToPod(ctx context.Context, namespace, pod, container, localDir, remoteDir string, buckets [][]RemoteFileEntry, progress *CopyProgress, compress bool) error {
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
			if err := c.runBucketUpload(ctx, cs, cfg, namespace, pod, container, localDir, remoteDir, files, progress, compress); err != nil {
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

func (c *Client) runBucketUpload(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container, localDir, remoteDir string, files []RemoteFileEntry, progress *CopyProgress, compress bool) error {
	if len(files) == 0 {
		return nil
	}

	pr, pw := io.Pipe()
	tarErrCh := make(chan error, 1)
	go func() {
		var sink io.Writer = pw
		var gz *gzip.Writer
		if compress {
			gz = gzip.NewWriter(pw)
			sink = gz
		}
		err := writeFilesTar(localDir, files, sink, progress)
		if gz != nil {
			if closeErr := gz.Close(); err == nil {
				err = closeErr
			}
		}
		_ = pw.Close()
		tarErrCh <- err
	}()

	tarCmd := "tar xf -"
	if compress {
		tarCmd = "tar xzf -"
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("%s -C %s", tarCmd, shellutil.Quote(remoteDir))}
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

// RemoteFileInfo is the minimal stat shape we need to plan a parallel
// single-file download. Size is the file's byte length; Mode is the POSIX
// mode bits (lower 9 bits only); IsRegular distinguishes regular files from
// directories, symlinks, sockets, etc.
type RemoteFileInfo struct {
	Size      int64
	Mode      os.FileMode
	IsRegular bool
}

// probeRemoteFile collects size, mode, and type for path in a single exec so
// we can decide how to transfer it without a follow-up round-trip. The
// output format is one line with three space-separated fields: SIZE MODE
// TYPE. TYPE is `regular` for regular files (anything else causes callers to
// refuse parallel chunking).
func (c *Client) probeRemoteFile(ctx context.Context, namespace, pod, container, path string) (*RemoteFileInfo, error) {
	// Prefer GNU stat for its predictable -c format; fall back to a portable
	// shell computation using `wc -c` (size), the mode bits derived from
	// `ls -l`, and a coarse type check via `test -f`.
	script := fmt.Sprintf(
		`p=%s; if stat -c '%%s %%a %%F' "$p" 2>/dev/null; then :; else s=$(wc -c <"$p" 2>/dev/null || echo 0); if [ -f "$p" ]; then t=regular; else t=other; fi; echo "$s 644 $t"; fi`,
		shellutil.Quote(path),
	)
	out, err := c.ExecShInContainer(ctx, namespace, pod, container, script)
	if err != nil {
		return nil, fmt.Errorf("probe %s: %w", path, err)
	}
	return parseProbeOutput(out)
}

// parseProbeOutput decodes the single-line probe emitted by probeRemoteFile.
// Extracted so we can unit-test the parser without round-tripping through
// exec.
func parseProbeOutput(raw []byte) (*RemoteFileInfo, error) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return nil, fmt.Errorf("empty probe output")
	}
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected probe output: %q", line)
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || size < 0 {
		return nil, fmt.Errorf("invalid size %q", fields[0])
	}
	// GNU stat prints mode as octal digits without a leading 0, so ParseUint
	// with base 8 picks up "644", "755", etc. correctly. We cap to the
	// low-9 permission bits — type bits are irrelevant for chmod on the
	// local side.
	mode, err := strconv.ParseUint(fields[1], 8, 32)
	if err != nil {
		mode = 0o644
	}
	// GNU stat's %F prints a human-readable type description. "regular file"
	// and "regular empty file" both start with "regular"; everything else
	// (directory, symlink, etc.) doesn't. The portable fallback in
	// probeRemoteFile emits "regular" or "other" directly.
	typ := strings.ToLower(strings.Join(fields[2:], " "))
	isReg := strings.HasPrefix(typ, "regular")
	return &RemoteFileInfo{
		Size:      size,
		Mode:      os.FileMode(mode) & os.ModePerm,
		IsRegular: isReg,
	}, nil
}

// minBytesForFileParallel is the file-size floor below which range splits
// cost more than they save: dd process startup plus the SPDY stream setup
// for each chunk dominates when each chunk would be a few MiB.
const minBytesForFileParallel = 16 * 1024 * 1024

// adaptiveFileParallelism chooses how many concurrent ranges to run given a
// file size and the user's requested cap. Small files get fewer workers so
// a 20 MiB download with --parallel 8 doesn't burn setup time on 8 tiny
// ranges. The schedule is intentionally coarse; the only hard rules are:
// never exceed `requested`, never go below 1, and always require that each
// worker own at least ~8 MiB of data so range-read overhead is amortized.
func adaptiveFileParallelism(size int64, requested int) int {
	if requested < 1 {
		requested = 1
	}
	if size < minBytesForFileParallel {
		return 1
	}
	// One worker per ~16 MiB of data, capped at requested.
	ideal := int(size / (16 * 1024 * 1024))
	if ideal < 1 {
		ideal = 1
	}
	if ideal > requested {
		ideal = requested
	}
	return ideal
}

// splitFileRanges divides [0,size) into n contiguous byte ranges. The last
// range absorbs the remainder so no bytes are dropped when size % n != 0.
// Returns offset/length pairs suitable for `dd iflag=skip_bytes,count_bytes`
// or `tail -c +OFFSET | head -c LENGTH`.
func splitFileRanges(size int64, n int) [][2]int64 {
	if n < 1 {
		n = 1
	}
	if size <= 0 {
		return nil
	}
	base := size / int64(n)
	out := make([][2]int64, 0, n)
	var off int64
	for i := 0; i < n; i++ {
		length := base
		if i == n-1 {
			length = size - off
		}
		if length <= 0 {
			continue
		}
		out = append(out, [2]int64{off, length})
		off += length
	}
	return out
}

// CopyFileFromPodParallelWithOptions downloads a single file from the pod
// using up to `parallel` concurrent byte-range streams. It falls back to
// CopyFromPodInContainerWithOptions (one exec, tar-wrapped) whenever:
//   - parallel <= 1 (explicitly sequential)
//   - the probe fails (can't determine size/mode)
//   - the file isn't regular (directory, symlink, etc.)
//   - the file is smaller than minBytesForFileParallel
//
// The parallel path is optimized for the high-RTT case where per-SPDY-stream
// flow control caps a single stream well below available bandwidth.
func (c *Client) CopyFileFromPodParallelWithOptions(ctx context.Context, namespace, pod, container, remotePath, localPath string, parallel int, opts CopyOptions) error {
	if parallel <= 1 {
		return c.CopyFromPodInContainerWithOptions(ctx, namespace, pod, container, remotePath, localPath, opts)
	}

	opts.Progress.phase("probing remote file")
	info, probeErr := c.probeRemoteFile(ctx, namespace, pod, container, remotePath)
	opts.Progress.phase("")
	if probeErr != nil || !info.IsRegular || info.Size < minBytesForFileParallel {
		return c.CopyFromPodInContainerWithOptions(ctx, namespace, pod, container, remotePath, localPath, opts)
	}
	eff := adaptiveFileParallelism(info.Size, parallel)
	if eff <= 1 {
		return c.CopyFromPodInContainerWithOptions(ctx, namespace, pod, container, remotePath, localPath, opts)
	}
	ranges := splitFileRanges(info.Size, eff)

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	// Pre-allocate the tmp file so worker WriteAt calls can land at their
	// final offsets without racing extensions. Using CreateTemp gives us a
	// unique name adjacent to the destination, matching the atomic-rename
	// pattern used elsewhere.
	tmp, err := createTempDownloadFile(localPath)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Truncate(info.Size); err != nil {
		cleanupTmp()
		return fmt.Errorf("preallocate %s: %w", tmpPath, err)
	}

	opts.Progress.fileStart(filepath.Base(localPath), info.Size)
	if err := c.runFileRangesDownload(ctx, namespace, pod, container, remotePath, tmp, ranges, opts.Progress); err != nil {
		cleanupTmp()
		return err
	}
	opts.Progress.fileEnd(filepath.Base(localPath))

	if err := tmp.Chmod(info.Mode | 0o600); err != nil {
		cleanupTmp()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// runFileRangesDownload fans out one exec stream per byte range, each
// reading its slice via `tail -c +OFFSET | head -c LENGTH` (universally
// portable) and writing into the pre-allocated tmp file at the matching
// offset via WriteAt. The first worker error cancels the rest.
func (c *Client) runFileRangesDownload(ctx context.Context, namespace, pod, container, remotePath string, dst *os.File, ranges [][2]int64, progress *CopyProgress) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, len(ranges))
	for _, r := range ranges {
		wg.Add(1)
		go func(offset, length int64) {
			defer wg.Done()
			if _, err := c.downloadFileRange(ctx, cs, cfg, namespace, pod, container, remotePath, dst, offset, length, progress); err != nil {
				errCh <- err
				cancel()
			}
		}(r[0], r[1])
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

// downloadFileRange pulls [offset, offset+length) from remotePath and writes
// it into dst at the matching offset. Returns the number of bytes actually
// written so callers (resume logic in particular) can distinguish
// "connection died after N bytes" from "connection died with zero progress".
// The remote command uses `tail -c +N | head -c L` instead of
// `dd iflag=skip_bytes` because the former works on GNU coreutils, BSD
// coreutils, and busybox alike; `dd`'s byte-level skip/count flags have
// patchy busybox support.
func (c *Client) downloadFileRange(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container, remotePath string, dst *os.File, offset, length int64, progress *CopyProgress) (int64, error) {
	// tail's `+N` argument is 1-indexed (start at byte N), so offset 0 maps
	// to `tail -c +1`. head -c bounds the output length exactly, which
	// means any over-read from tail's buffering is discarded harmlessly.
	script := fmt.Sprintf(
		"tail -c +%d %s | head -c %d",
		offset+1,
		shellutil.Quote(remotePath),
		length,
	)
	cmd := []string{"sh", "-lc", script}

	pr, pw := io.Pipe()
	var errBuf bytes.Buffer
	execErrCh := make(chan error, 1)
	go func() {
		err := c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, nil, pw, &errBuf, false)
		_ = pw.CloseWithError(err)
		execErrCh <- err
	}()

	// offsetWriter writes to dst at a specific base offset, advancing with
	// each Write. It also reports to progress so the shared CopyProgress
	// ticks up as bytes land on disk (not just arrive over the wire).
	w := &offsetWriter{dst: dst, offset: offset, progress: progress}
	copied, copyErr := io.Copy(w, pr)
	_ = pr.Close()
	execErr := <-execErrCh
	if copyErr != nil {
		return copied, fmt.Errorf("range [%d,%d) copy: %w", offset, offset+length, copyErr)
	}
	if execErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return copied, fmt.Errorf("range [%d,%d) exec: %w: %s", offset, offset+length, execErr, msg)
		}
		return copied, fmt.Errorf("range [%d,%d) exec: %w", offset, offset+length, execErr)
	}
	if copied != length {
		return copied, fmt.Errorf("range [%d,%d) delivered %d bytes", offset, offset+length, copied)
	}
	return copied, nil
}

// offsetWriter is a scratch io.Writer that writes via WriteAt into a shared
// *os.File at a moving offset. Workers own disjoint ranges so there's no
// overlap and WriteAt is safe to call concurrently per POSIX.
type offsetWriter struct {
	dst      *os.File
	offset   int64
	progress *CopyProgress
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.dst.WriteAt(p, w.offset)
	if n > 0 {
		w.offset += int64(n)
		w.progress.bytes(n)
	}
	return n, err
}
