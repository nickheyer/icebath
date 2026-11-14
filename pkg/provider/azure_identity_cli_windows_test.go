//go:build windows

package provider

import (
	"os"
	"path/filepath"
	"testing"
)

// Writes an az.cmd batch file that prints a token or the az login error
func writeFakeAz(t *testing.T, dir string, succeed bool) {
	t.Helper()
	body := "@echo off\r\n>&2 echo ERROR: Please run 'az login' to setup account.\r\nexit /b 1\r\n"
	if succeed {
		body = "@echo off\r\necho {\"accessToken\":\"cli-token\",\"expires_on\":1999999999,\"resource\":\"%~6\"}\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "az.cmd"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
