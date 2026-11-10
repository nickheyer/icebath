package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Usage, help and validation output byte for byte as Python 3.14 argparse prints it for the same arguments
func TestBuildEnvArgparseMatchesPython(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "build_env_argparse.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Argv    []string
		Columns string
		Colors  string
		Code    int
		Stdout  string
		Stderr  string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(strings.Join(c.Argv, " ")+"@"+c.Columns+c.Colors, func(t *testing.T) {
			for _, k := range []string{"FORCE_COLOR", "NO_COLOR", "PYTHON_COLORS", "TERM"} {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			t.Setenv("COLUMNS", c.Columns)
			if c.Colors != "" {
				t.Setenv("PYTHON_COLORS", c.Colors)
			}
			var stdout, stderr bytes.Buffer
			code := BuildEnvMain(context.Background(), c.Argv, "build_env.py", &stdout, &stderr)
			if code != c.Code || stdout.String() != c.Stdout || stderr.String() != c.Stderr {
				t.Fatalf("code %d want %d\nstdout %q\nwant   %q\nstderr %q\nwant   %q", code, c.Code, stdout.String(), c.Stdout, stderr.String(), c.Stderr)
			}
		})
	}
}

// Paths print the way str of pathlib Path normalizes them
func TestBuildEnvPathText(t *testing.T) {
	for in, want := range map[string]string{"": ".", "a//b/": "a/b", "./a": "a", "/x/./y": "/x/y", "//x": "//x", "///x": "/x", "a/../b": "a/../b"} {
		if got := pyPathText(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

// The exact lock prune runs from the command and prints its count
func TestBuildEnvExactLockPrunesAndReports(t *testing.T) {
	root := t.TempDir()
	peWrite(t, filepath.Join(root, "repo", "uv.lock"), "[[package]]\nname = \"keep-me\"\nversion = \"1.0\"\n")
	writeFile(t, filepath.Join(root, "cache", "archive-v0", "gone", "gone_dep-1.0.dist-info"), nil, 0o644)
	var stdout, stderr bytes.Buffer
	code := BuildEnvMain(context.Background(), []string{"--exact-lock", "--lock-source", filepath.Join(root, "repo"), "--cache", filepath.Join(root, "cache")}, "build_env.py", &stdout, &stderr)
	if code != 0 || stdout.String() != "pruned 1 cache entries outside the lock\n" || stderr.Len() != 0 {
		t.Fatalf("code %d out %q err %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	code = BuildEnvMain(context.Background(), []string{"--exact-lock", "--lock-source", filepath.Join(root, "missing"), "--cache", filepath.Join(root, "cache")}, "build_env.py", &stdout, &stderr)
	if code != 1 || !strings.HasPrefix(stderr.String(), "python environment: ") {
		t.Fatalf("missing lock: code %d err %q", code, stderr.String())
	}
}

func TestExportedArgparseTheme(t *testing.T) {
	t.Setenv("PYTHON_COLORS", "0")
	if colors := ArgparseThemeFor(&bytes.Buffer{}); colors != (ArgparseColors{}) {
		t.Fatalf("uncolored theme %+v", colors)
	}
	t.Setenv("PYTHON_COLORS", "1")
	if colors := ArgparseThemeFor(&bytes.Buffer{}); colors.Usage != "\x1b[1;34m" || colors.Reset != "\x1b[0m" {
		t.Fatalf("colored theme %+v", colors)
	}
	t.Setenv("COLUMNS", "100")
	if width := ArgparseWidth(); width != 98 {
		t.Fatalf("width %d", width)
	}
}
