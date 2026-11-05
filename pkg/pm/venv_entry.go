package pm

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"os"
	"path/filepath"
)

//go:embed venv_entry.py
var venvEntrySource []byte

// Writes an embedded script once under a directory named by its digest and returns its path
func materializeScript(ctx context.Context, name string, body []byte) (string, error) {
	sum := sha256.Sum256(body)
	dir := filepath.Join(InstallStateDir(ctx, RepoRoot()), "pm-scripts", hex.EncodeToString(sum[:8]))
	path := filepath.Join(dir, name)
	if current, err := os.ReadFile(path); err == nil && string(current) == string(body) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	if err := DurableWriteBytes(path, body); err != nil {
		return "", err
	}
	return path, nil
}

// Path of the script that attaches a venv's site packages to another interpreter
func VenvEntryScript(ctx context.Context) (string, error) {
	return materializeScript(ctx, "_venv_entry.py", venvEntrySource)
}

// Argv prefix running code inside a venv through another interpreter, a script, -c or -m following it
func VenvEntryCommand(ctx context.Context, python string, options []string, sitePackages string) ([]string, error) {
	entry, err := VenvEntryScript(ctx)
	if err != nil {
		return nil, err
	}
	flags := append([]string{}, options...)
	if !containsString(flags, "-S") {
		flags = append(flags, "-S")
	}
	return append(append([]string{python}, flags...), entry, sitePackages), nil
}
