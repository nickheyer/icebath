package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Module path a source checkout of icebath declares in its go.mod
const icebathModule = "github.com/nickheyer/icebath"

// Whether dir holds the icebath module with its command package
func isIcebathCheckout(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) == 2 && fields[0] == "module" {
			st, err := os.Stat(filepath.Join(dir, "cmd", "icebath"))
			return fields[1] == icebathModule && err == nil && st.IsDir()
		}
	}
	return false
}

// Nearest icebath source checkout above the running binary, then above the working directory
func icebathSourceCheckout() (string, bool) {
	var starts []string
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, dir := range starts {
		for {
			if isIcebathCheckout(dir) {
				return dir, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", false
}

// Arguments of this launch without the dev flag the source run would otherwise repeat
func withoutDevFlag(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i, arg := range argv {
		if arg == "--" {
			return append(out, argv[i:]...)
		}
		if arg != "--dev" {
			out = append(out, arg)
		}
	}
	return out
}

// Compiles the icebath Go sources with the release build tags, runs the TUI from them and returns its exit status
func runTUIFromSource(argv []string) int {
	root, ok := icebathSourceCheckout()
	if !ok {
		fmt.Fprintln(os.Stderr, "Error: --dev runs the TUI from the icebath Go sources, but this icebath is not inside a source checkout.\n"+
			"Run `icebath --dev` from a checkout of the icebath repository (or a binary built inside one).")
		return 1
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: Go is required for --dev but `go` was not found. Install it from https://go.dev/dl, "+
			"then retry `icebath --dev`. To keep working now, run `icebath`.")
		return 1
	}
	tmp, err := os.MkdirTemp("", "icebath-dev-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not prepare the source build: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	exe := filepath.Join(tmp, "icebath")
	build := exec.Command(goBin, "build", "-tags", "goolm", "-o", exe, "./cmd/icebath")
	build.Dir = root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: building the TUI from %s failed: %v\n", root, err)
		return 1
	}
	cmd := exec.Command(exe, withoutDevFlag(argv)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	case err != nil:
		fmt.Fprintf(os.Stderr, "Error: could not run the TUI from %s: %v\n", root, err)
		return 1
	}
	return 0
}
