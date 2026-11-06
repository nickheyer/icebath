package selfupdate

import "golang.org/x/sys/unix"

// Makes this process the child subreaper, so orphaned descendants are re-parented to it
var custodySubreaper = func() bool { return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) == nil }
