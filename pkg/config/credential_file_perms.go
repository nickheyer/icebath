package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"

	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Warns with a chmod 600 hint when a POSIX credential file is group or world readable, true when it warned
func WarnIfCredentialFileBroadlyReadable(path, label string, log *slog.Logger) bool {
	shown := filepath.Clean(path)
	mode, ok := fsutil.PermBits(shown)
	if !ok {
		return false
	}
	if runtime.GOOS == "windows" || mode&(0o040|0o004) == 0 {
		return false
	}
	if log == nil {
		log = pkgLog()
	}
	prefix := ""
	if label != "" {
		prefix = label + " "
	}
	log.Warn(fmt.Sprintf("%s%s is group/world-readable (mode 0%o) and contains secrets. Run: chmod 600 %s", prefix, pathutil.Name(shown), mode, shown))
	return true
}
