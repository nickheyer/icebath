package backup

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestIterBackupFilesSkipsUnixSockets(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	os.MkdirAll(root, 0o755)
	t.Chdir(root)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", "gateway.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if sel := selectedRels(root, filepath.Join(tmp, "out.zip"), nil); sel["gateway.sock"] {
		t.Fatal("selected a socket")
	}
}
