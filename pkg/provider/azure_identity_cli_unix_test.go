//go:build !windows

package provider

import (
	"os"
	"path/filepath"
	"testing"
)

// Writes an az shell script that prints a token or the az login error
func writeFakeAz(t *testing.T, dir string, succeed bool) {
	t.Helper()
	body := "#!/bin/sh\necho \"ERROR: Please run 'az login' to setup account.\" >&2\nexit 1\n"
	if succeed {
		body = "#!/bin/sh\nfor a in \"$@\"; do last=\"$a\"; done\necho '{\"accessToken\":\"cli-token\",\"expires_on\":1999999999,\"resource\":\"'$last'\"}'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
