package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Canary rule of pkg/selfupdate IsCanaryTag, carried here so the generator builds without the main module
var canaryTagRE = regexp.MustCompile(`^v(?:0|[1-9]\p{Nd}{0,2})\.(?:0|[1-9]\p{Nd}*)\.(?:0|[1-9]\p{Nd}*)\+canary\.20\p{Nd}{6}T\p{Nd}{6}Z$`)

var commitRE = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Plate fills of the stable, canary and commit build identities
var (
	stablePlates = [2]string{"#ffffff", "#0b0f14"}
	canaryPlates = [2]string{"#f5cc32", "#443808"}
	commitPlates = [2]string{"#e34850", "#4a1117"}
)

// Loads the source art and returns the stable identity and the one desktop outputs carry
func buildArt(source string, env func(string) string) (*art, *art, error) {
	tag, commit := env("ICEBATH_PAYLOAD_TAG"), env("ICEBATH_BUILD_COMMIT")
	var desktopPlates [2]string
	switch {
	case commit != "" && tag != "":
		return nil, nil, errors.New("Commit builds cannot also select ICEBATH_PAYLOAD_TAG")
	case commit != "" && !commitRE.MatchString(commit):
		return nil, nil, errors.New("ICEBATH_BUILD_COMMIT requires an exact full 40-character SHA")
	case commit != "":
		desktopPlates = commitPlates
	case canaryTagRE.MatchString(strings.TrimSpace(tag)):
		desktopPlates = canaryPlates
	}
	src, err := loadSources(source)
	if err != nil {
		return nil, nil, err
	}
	stable := newArt(src, stablePlates, "")
	if desktopPlates == ([2]string{}) {
		return stable, stable, nil
	}
	return stable, newArt(src, desktopPlates, commit), nil
}

// Desktop outputs carry the build identity, every other output keeps the stable brand
func artFor(rel string, stable, desktop *art) *art {
	if strings.HasPrefix(rel, "apps/desktop/") {
		return desktop
	}
	return stable
}

// Writes every target under out, reports each one, then verifies what landed and fails on any error
func cmdWrite(stdout io.Writer, targets []target, stable, desktop *art, out string) int {
	written, failures := 0, 0
	for _, t := range targets {
		data, err := t.render(artFor(t.rel, stable, desktop))
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
func cmdCheck(stdout io.Writer, targets []target, stable, desktop *art, out string) int {
	var problems []string
	for _, t := range targets {
		fresh, err := t.render(artFor(t.rel, stable, desktop))
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
func run(args []string, stdout, stderr io.Writer, env func(string) string) int {
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
	stable, desktop, err := buildArt(root, env)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *check {
		return cmdCheck(stdout, allTargets(), stable, desktop, dest)
	}
	return cmdWrite(stdout, allTargets(), stable, desktop, dest)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}
