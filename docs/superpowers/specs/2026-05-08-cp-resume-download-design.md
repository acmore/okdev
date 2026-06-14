# Resumable Single-File `okdev cp` Downloads

## Goal

Make `okdev cp :/remote/file ./local` resilient to flaky stream failures by resuming from already-downloaded local bytes instead of restarting from byte 0 on every retry or rerun.

## Scope

This design covers:

- single-file downloads from a session pod to a local path
- automatic resume across retryable failures inside one `okdev cp` run
- resume when the user reruns the same command later and a partial local file already exists
- user-facing progress behavior for resumed downloads

This design does not cover:

- uploads
- directory downloads
- multi-pod downloads
- checksum validation of partially downloaded content

## Current Behavior

Today single-file downloads use a `tar czf -` stream from the remote pod and extract a single file into a local temp file. The temp file is renamed into place only after extraction completes.

If the stream ends with a retryable error such as `unexpected EOF`, `connection reset by peer`, or `timeout`, `okdev cp` retries the whole transfer. Because the remote stream always starts from byte 0, the retry redownloads the entire file. This is especially costly for large artifacts on unstable connections.

There is also a late-error edge case: the local file may already have been fully written and renamed before the remote exec stream reports a retryable error. The current retry loop can then restart a transfer that was already effectively complete.

## Constraints

- Keep the change narrow and preserve current behavior for uploads, directories, and multi-pod copies.
- Continue to rely on `sh -lc` remote execution so the feature works through the existing Kubernetes exec path.
- Avoid assuming a writable shared temp directory outside the destination directory; partial files should live beside the user-requested destination.
- Preserve current local permission behavior as closely as possible.

## Proposed Approach

Add a dedicated resumable download path for single-file downloads. This path replaces the current tar-based single-file download flow only for `remote file -> local file` copies.

The resumable path will:

1. Probe the remote file metadata before starting data transfer.
2. Resolve a canonical local partial file path beside the final destination.
3. Determine the already-committed local byte count from an existing partial or undersized final file.
4. Stream only the remaining remote byte range and append it locally.
5. Retry from the new committed local size after retryable failures.
6. Promote the completed partial file to the final destination once the local size matches the expected remote size.

## Remote Metadata Probe

Before downloading, `okdev cp` should query the remote file for:

- file size in bytes
- file mode, when available

The probe should fail if the remote path is not a regular file.

The implementation should prefer simple shell primitives available in typical Linux containers:

- `wc -c < "$file"` for size
- `stat -c '%a' "$file"` for mode when available

If mode probing fails, the local file should fall back to the existing default file mode behavior of `0644`.

## Local Partial File Rules

The canonical partial path is:

```text
<localPath>.okdev-part
```

Resume state is derived from files already on disk:

- If only `<localPath>` exists and its size equals the remote size, treat the copy as already complete.
- If only `<localPath>.okdev-part` exists and its size equals the remote size, promote it to `<localPath>` and treat the copy as complete.
- If only `<localPath>` exists and its size is smaller than the remote size, rename it to `<localPath>.okdev-part` and resume from that size.
- If only `<localPath>.okdev-part` exists and its size is smaller than the remote size, resume from that size.
- If both `<localPath>` and `<localPath>.okdev-part` exist and both are smaller than the remote size, keep the larger one as the partial file and remove the smaller one.
- If both exist and one already matches the remote size, keep the complete file as `<localPath>` and remove the incomplete one.
- If either local file is larger than the remote file, fail with a clear message instead of guessing.

This keeps the feature compatible with both the new partial-file workflow and older interrupted copies that left a truncated final destination.

## Remote Data Streaming

The resumable single-file path should stream raw file bytes instead of tar output.

For a given resume offset `N`, the remote command should emit bytes starting at offset `N` and continuing to EOF. The initial implementation should use a shell command based on `dd`:

```sh
dd if="$file" bs=1 skip="$offset"
```

The local side should open the partial file in append mode and write the stream directly into it.

This keeps the protocol simple:

- no temp tar archive
- no decompression
- no post-extract file discovery
- explicit local byte accounting

## Retry Semantics

Resume applies both within a single invocation and across separate invocations.

Within one invocation:

- after each retryable stream failure, re-check the partial file size on disk
- restart the remote stream from that local size
- stop retrying once the local partial size reaches the expected remote size

Across separate invocations:

- probe remote size again
- reuse the existing partial size if it is smaller than the remote size
- continue from that offset

Late retryable exec errors after the local partial file has already reached the expected remote size should be treated as success, not as a reason to restart the transfer.

## Finalization

When the local partial file size equals the expected remote size:

- apply the probed remote mode if available, otherwise `0644`
- atomically rename `<localPath>.okdev-part` to `<localPath>`

If `<localPath>` already exists at finalization time, it should be replaced by the completed partial file.

## User Experience

User-visible behavior should make resume obvious:

- if a resumed transfer starts from a non-zero offset, print a one-time persistent line such as `Resuming download from 70.0 MiB`
- initialize the progress byte counter from the existing committed local byte count so the progress display reflects total local progress rather than only newly transferred bytes

If the destination is already complete, `okdev cp` should return success without redownloading and print a short confirmation that the local file already matches the remote size.

## Failure Modes

The command should fail clearly when:

- the remote path is missing or not a regular file
- the existing local file or partial file is larger than the remote file
- the local destination and partial file conflict in a way that cannot be resolved safely
- finalization fails due to local filesystem errors

The first version intentionally does not validate checksums of existing partial content. If the remote file changes without a size change between attempts, `okdev cp` may append to stale local bytes. This is an accepted limitation for v1; callers should use this feature primarily for immutable or append-stable artifacts.

## Testing

Add focused tests for:

- promoting an undersized final destination into the canonical partial file
- preferring the larger local partial source when both final and partial files exist
- rejecting oversized local files
- resuming a partial local file to completion
- treating a full local file as already complete
- treating a late retryable stream error after full local download as success
- preserving retry behavior for transient failures while advancing the resume offset

Keep existing directory and upload tests unchanged to ensure scope remains narrow.

## Documentation Changes

When the implementation lands, update the `okdev cp` command reference to say:

- single-file downloads resume from existing partial local state
- directory and multi-pod downloads do not yet support resume

## Recommended Implementation Boundary

Keep the new behavior isolated in the kube copy layer:

- add resumable helpers for probing remote metadata and streaming a file range
- keep `internal/cli/cp.go` responsible only for selecting the single-file download path and surfacing resume status to the progress UI

This limits behavioral change to the copy code and avoids mixing resume logic into unrelated CLI selection code.
