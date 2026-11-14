package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitRootForIpcReturnsNullForInvalidAndDevicePaths(t *testing.T) {
	for _, v := range []any{"", "   ", nil, `\\?\C:\secret`, "file:///%E0%A4%A"} {
		if got := GitRootForIpc(v); got != nil {
			t.Fatal(v, *got)
		}
	}
}

func TestGitRootForIpcResolvesDirectoriesFilesMissingDescendantsAndFileURLs(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	file := filepath.Join(src, "index.ts")
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	writeFile(t, file, "export {}\n")
	for _, v := range []string{root, src, file, pathToFileURL(file), filepath.Join(src, "missing.ts")} {
		if got := GitRootForIpc(v); got == nil || *got != root {
			t.Fatal(v, got)
		}
	}
}
