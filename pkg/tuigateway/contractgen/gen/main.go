package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway/contractgen"
)

// Writes the generated contract files and their digests, or with -check lists the stale ones and fails
func main() {
	check := flag.Bool("check", false, "report stale files without writing")
	root := flag.String("root", filepath.Join("..", ".."), "repository root")
	flag.Parse()
	files, err := contractgen.Render()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var stale []string
	for _, p := range paths {
		full := filepath.Join(*root, filepath.FromSlash(p))
		current, err := os.ReadFile(full)
		if err == nil && strings.TrimPrefix(string(current), "\ufeff") == files[p] {
			continue
		}
		if *check {
			stale = append(stale, p)
			continue
		}
		if err := os.WriteFile(full, []byte(files[p]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("wrote " + p)
	}
	digests := filepath.Join(*root, "pkg", "tuigateway", "contractgen", "digest.go")
	text := contractgen.DigestSource(files)
	if current, err := os.ReadFile(digests); err != nil || string(current) != text {
		if *check {
			stale = append(stale, "pkg/tuigateway/contractgen/digest.go")
		} else if err := os.WriteFile(digests, []byte(text), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	for _, p := range stale {
		fmt.Fprintln(os.Stderr, "stale: "+p+" — run go generate ./pkg/tuigateway")
	}
	if len(stale) > 0 {
		os.Exit(1)
	}
}
