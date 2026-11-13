package home

// macOS caps AF_UNIX paths at 104 bytes and its per-user TMPDIR never fits, so sockets always go under /tmp
const forcedSocketRoot = "/tmp"
