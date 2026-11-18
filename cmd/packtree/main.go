// Command packtree packs and unpacks deterministic zstd tars of file trees with their manifests and renders the plugin catalog document
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  packtree pack -dir DIR -out ARCHIVE [-manifest FILE] [-shebang-exec]
  packtree unpack -archive ARCHIVE -dir DIR
  packtree catalog -dir DIR -out FILE                                  write the catalog document of a plugin catalog directory`)
	os.Exit(2)
}

// Dispatches a verb
func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "pack":
		err = packVerb(os.Args[2:])
	case "unpack":
		err = unpackVerb(os.Args[2:])
	case "catalog":
		err = catalogVerb(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fail(err)
	}
}

// Packs a directory into an archive and its manifest
func packVerb(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	dir := fs.String("dir", "", "tree to pack")
	out := fs.String("out", "", "archive to write")
	manifest := fs.String("manifest", "", "manifest to write, none when empty")
	shebangExec := fs.Bool("shebang-exec", false, "pack files that start with #! as executable")
	fs.Parse(args)
	if *dir == "" || *out == "" {
		return fmt.Errorf("pack needs -dir and -out")
	}
	files, err := tarzst.Collect(*dir, *shebangExec)
	if err != nil {
		return err
	}
	return writeArchive(*out, *manifest, files)
}

// Writes every archived file under a directory with its mode
func unpackVerb(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ExitOnError)
	archive := fs.String("archive", "", "archive to read")
	dir := fs.String("dir", "", "directory to write")
	fs.Parse(args)
	if *archive == "" || *dir == "" {
		return fmt.Errorf("unpack needs -archive and -dir")
	}
	data, err := os.ReadFile(*archive)
	if err != nil {
		return err
	}
	return tarzst.Extract(data, *dir)
}

// Encodes the files into the archive and writes the manifest when one is named
func writeArchive(out, manifest string, files []tarzst.File) error {
	archive, err := tarzst.Encode(files)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, archive, 0o644); err != nil {
		return err
	}
	if manifest == "" {
		return nil
	}
	return os.WriteFile(manifest, []byte(tarzst.Manifest(files)), 0o644)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "packtree:", err)
	os.Exit(1)
}
