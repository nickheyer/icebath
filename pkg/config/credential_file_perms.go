package config

import (
	"fmt"
	"log/slog"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Warns with a chmod 600 hint when a POSIX credential file is group or world readable, true when it warned
func WarnIfCredentialFileBroadlyReadable(path, label string, log *slog.Logger) bool {
	shown := pypath.PurePathStr(path)
	mode, ok := pyfs.PreserveFileMode(shown)
	if !ok {
		return false
	}
	if runtime.GOOS == "windows" || mode&(0o040|0o004) == 0 {
		return false
	}
	if log == nil {
		log = slog.Default().With("logger", "utils")
	}
	prefix := ""
	if label != "" {
		prefix = label + " "
	}
	log.Warn(fmt.Sprintf("%s%s is group/world-readable (mode 0%o) and contains secrets. Run: chmod 600 %s", prefix, pypath.PathName(shown), mode, shown))
	return true
}
