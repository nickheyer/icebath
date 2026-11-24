package tuiserver

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Appends one headed entry to the crash log with the detail dump writes, never raising
func appendCrashLog(header string, dump func(io.Writer)) {
	suppress(func() {
		path := crashLogPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "\n=== %s ===\n", header)
		if dump != nil {
			dump(f)
		}
	})
}
