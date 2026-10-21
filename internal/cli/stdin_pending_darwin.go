package cli

// fionread asks how many bytes a pipe holds without reading them:
// FIONREAD = _IOR('f', 127, int), which golang.org/x/sys/unix does not export
// for darwin.
const fionread = 0x4004667f
