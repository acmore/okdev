package cli

import "golang.org/x/sys/unix"

// fionread asks how many bytes a pipe holds without reading them; on Linux
// FIONREAD is TIOCINQ.
const fionread = unix.TIOCINQ
