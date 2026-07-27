package cli

import "fmt"

// procStatGlob is the default file list the group scan reads. Injectable so
// tests can exercise the scan against a fake /proc.
const procStatGlob = "/proc/[0-9]*/stat"

// procGroupMembersScript renders the shell that sets `members` to the live
// pids whose process group is `$pgid`. Three call sites need exactly this —
// `jobs stop`, the --kill-group-on-exit cascade, and the exec-jobs liveness
// probe — and each carried its own copy of the awk before.
//
// The file list is piped through `cat` rather than passed to awk as
// arguments, and that is the whole point rather than a style choice: a pid
// that exits between the shell expanding the glob and awk opening the file
// leaves an unreadable path in the list, and mawk (the default awk on
// Debian/Ubuntu, hence on CI and most dev images) aborts the entire scan at
// that file instead of skipping it. Every member after the vanished pid then
// goes unseen, `members` comes back short or empty, and the caller silently
// degrades to signaling the job leader alone — leaving the process group it
// meant to kill alive while reporting success. `cat` reports the missing file
// on stderr (discarded) and keeps going, so one vanished pid costs one line
// instead of the rest of the scan.
//
// The awk itself: strip the leading "<pid> (comm) " so a comm containing
// spaces or parens cannot shift the field offsets, then match on state (skip
// zombies) and pgrp. The pid comes from the line's first field, so nothing
// depends on the file name.
func procGroupMembersScript(fileList string) string {
	if fileList == "" {
		fileList = procStatGlob
	}
	return fmt.Sprintf(
		"members=$(cat %s 2>/dev/null | awk -v g=\"$pgid\" '{ pid=$1; line=$0; sub(/^[0-9]+ \\(.*\\) /, \"\", line); split(line, f, \" \"); if (f[1] != \"Z\" && f[3] == g) print pid }')",
		fileList,
	)
}
