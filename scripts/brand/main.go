package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Loads the source art the targets render from
func buildArt(source string) (*art, error) {
	src, err := loadSources(source)
	if err != nil {
		return nil, err
	}
	return &art{src: src}, nil
}

// Writes every target under out, reports each one, then verifies what landed and fails on any error
func cmdWrite(stdout io.Writer, targets []target, a *art, out string) int {
	written, failures := 0, 0
	for _, t := range targets {
		data, err := t.render(a)
		if err == nil {
			path := filepath.Join(out, filepath.FromSlash(t.rel))
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				err = os.WriteFile(path, data, 0o644)
			}
		}
		if err != nil {
			failures++
			fmt.Fprintf(stdout, "  !! %s: FAILED (%v)\n", t.rel, err)
			continue
		}
		written++
	}
	fmt.Fprintf(stdout, "[write] wrote %d/%d files\n", written, len(targets))
	fmt.Fprintln(stdout, "\n[verify]")
	for _, t := range targets {
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(t.rel)))
		if err == nil {
			var desc string
			if desc, err = describe(t.want, data); err == nil {
				fmt.Fprintf(stdout, "  %s: %s\n", t.rel, desc)
				continue
			}
		}
		failures++
		fmt.Fprintf(stdout, "  %s: VERIFY FAILED (%v)\n", t.rel, err)
	}
	if failures > 0 {
		return 1
	}
	return 0
}

// Regenerates every target in memory and checks the files under out against their declared shape
func cmdCheck(stdout io.Writer, targets []target, a *art, out string) int {
	var problems []string
	for _, t := range targets {
		fresh, err := t.render(a)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: REGENERATE FAILED (%v)", t.rel, err))
			continue
		}
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(t.rel)))
		if errors.Is(err, os.ErrNotExist) {
			problems = append(problems, fmt.Sprintf("%s: MISSING (expected generated file)", t.rel))
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: UNREADABLE (%v)", t.rel, err))
			continue
		}
		if t.want.format == "TEXT" && !bytes.Equal(fresh, data) {
			problems = append(problems, fmt.Sprintf("%s: STALE (differs from the regenerated source)", t.rel))
			continue
		}
		if err := inspect(t.want, data); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", t.rel, err))
		}
	}
	if len(problems) == 0 {
		fmt.Fprintf(stdout, "[ok] all %d targets generate and pass structural checks\n", len(targets))
		return 0
	}
	fmt.Fprintf(stdout, "[check] %d problem(s):\n", len(problems))
	for _, line := range problems {
		fmt.Fprintf(stdout, "  - %s\n", line)
	}
	return 1
}

// Walks up from dir to the checkout root that holds the source art
func findRoot(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "assets", logoFile)); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no %s above %s, pass --source", filepath.Join("assets", logoFile), dir)
		}
	}
}

// Parses flags, resolves the identity, then writes or checks every target and returns the exit code
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("brand", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "checkout root holding assets/ (default: nearest one above the working directory)")
	out := fs.String("out", "", "output root (default: the source root)")
	check := fs.Bool("check", false, "regenerate in memory and check the outputs under --out")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root := *source
	if root == "" {
		wd, err := os.Getwd()
		if err == nil {
			root, err = findRoot(wd)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	dest := root
	if *out != "" {
		if dest, err = filepath.Abs(*out); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	a, err := buildArt(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		return cmdCheck(stdout, allTargets(), a, dest)
	}
	return cmdWrite(stdout, allTargets(), a, dest)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
