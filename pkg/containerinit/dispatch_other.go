//go:build !linux

package containerinit

// Container verbs exist only in the Linux image, every command line is an ordinary one elsewhere
func Dispatch(argv []string) (int, bool) { return 0, false }

// The image privilege drop applies to the Linux image only
func DropRootInImage(argv []string) {}
