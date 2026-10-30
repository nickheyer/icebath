package vsc

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Extension id and, in builds made with make vsix, the extension package the binary installs into the editor
//
//go:embed bundle
var bundle embed.FS

// Longest the editor command line gets to install the extension
const installTimeout = 2 * time.Minute

// Id of the extension as the bundle names it, publisher dot name
func extensionID() string {
	data, err := bundle.ReadFile("bundle/extension")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Extension package built into the binary with its version, false when the build carried none
func bundledVSIX() ([]byte, string, bool) {
	data, err := bundle.ReadFile("bundle/icebath.vsix")
	if err != nil {
		return nil, "", false
	}
	version, err := vsixVersion(data)
	if err != nil {
		return nil, "", false
	}
	return data, version, true
}

// Version named by the package manifest inside an extension package
func vsixVersion(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name != "extension/package.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(rc).Decode(&pkg); err != nil {
			return "", err
		}
		if pkg.Version == "" {
			return "", errors.New("vsix manifest names no version")
		}
		return pkg.Version, nil
	}
	return "", errors.New("vsix holds no extension/package.json")
}

// Reports whether version a is above version b, comparing dotted numbers left to right
func newerVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(strings.TrimSpace(as[i]))
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(strings.TrimSpace(bs[i]))
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// Command line names of VS Code and its forks, the one the terminal hints at first
func cliNames(hint string) []string {
	names := []string{"code", "code-oss", "code-insiders", "cursor", "windsurf", "codium", "codium-insiders"}
	hint = strings.ToLower(hint)
	for _, want := range []string{"codium", "windsurf", "cursor", "insiders", "code-oss", "code - oss"} {
		if !strings.Contains(hint, want) {
			continue
		}
		var first, rest []string
		for _, n := range names {
			if strings.Contains(n, strings.ReplaceAll(want, " - ", "-")) || want == "insiders" && strings.HasSuffix(n, "-insiders") {
				first = append(first, n)
			} else {
				rest = append(rest, n)
			}
		}
		return append(first, rest...)
	}
	return names
}

// Command line of the editor the terminal belongs to, found beside its git helper, then on the PATH
func editorCLI() (string, bool) {
	hints := []string{os.Getenv("VSCODE_GIT_ASKPASS_MAIN"), os.Getenv("VSCODE_GIT_ASKPASS_NODE")}
	names := cliNames(strings.Join(hints, " "))
	for _, hint := range hints {
		if hint == "" {
			continue
		}
		for _, root := range ancestors(hint, 4) {
			if cli, ok := cliIn(filepath.Join(root, "bin"), names); ok {
				return cli, true
			}
		}
	}
	for _, name := range names {
		if cli, err := exec.LookPath(name); err == nil {
			return cli, true
		}
	}
	return "", false
}

// Parent directories of a path from the nearest up to the given count
func ancestors(path string, count int) []string {
	var out []string
	dir := filepath.Dir(path)
	for i := 0; i < count; i++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		out = append(out, dir)
		dir = parent
	}
	return out
}

// First of the names present as a file in the directory, with the script suffix on windows
func cliIn(dir string, names []string) (string, bool) {
	for _, name := range names {
		if runtime.GOOS == "windows" {
			name += ".cmd"
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// Installs the extension built into the binary through the editor command line
func installExtension(ctx context.Context) (string, error) {
	data, _, ok := bundledVSIX()
	if !ok {
		return "", errors.New("this build carries no VS Code extension, build it with make vsix")
	}
	cli, ok := editorCLI()
	if !ok {
		return "", errors.New("no code command line found beside the editor or on the PATH")
	}
	f, err := os.CreateTemp("", "icebath-*.vsix")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, cli, "--install-extension", f.Name(), "--force").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --install-extension: %w, %s", filepath.Base(cli), err, strings.TrimSpace(string(out)))
	}
	return cli, nil
}
