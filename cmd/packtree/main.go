// Command packtree writes a deterministic zstd tar of a file tree and its path, sha256 and mode manifest
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Packs -src into -out, or repacks the existing -out when no source is given, then writes -manifest when named
func main() {
	src := flag.String("src", "", "tree to pack, the existing archive being repacked when empty")
	out := flag.String("out", "", "archive to write")
	manifest := flag.String("manifest", "", "manifest to write, none when empty")
	shebangExec := flag.Bool("shebang-exec", false, "pack files that start with #! as executable")
	flag.Parse()
	if *out == "" {
		fail(fmt.Errorf("-out is required"))
	}
	var files []tarzst.File
	var err error
	if *src != "" {
		files, err = tarzst.Collect(*src, *shebangExec)
	} else {
		var data []byte
		if data, err = os.ReadFile(*out); err == nil {
			files, err = tarzst.Decode(data)
		}
	}
	if err != nil {
		fail(err)
	}
	archive, err := tarzst.Encode(files)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, archive, 0o644); err != nil {
		fail(err)
	}
	if *manifest == "" {
		return
	}
	if err := os.WriteFile(*manifest, []byte(tarzst.Manifest(files)), 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "packtree:", err)
	os.Exit(1)
}
