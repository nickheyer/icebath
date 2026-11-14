// Command packtree packs, unpacks and renames deterministic zstd tars of file trees with their path, sha256 and mode manifests
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  packtree [-src DIR] -out ARCHIVE [-manifest FILE] [-shebang-exec]   pack DIR, or repack ARCHIVE when -src is empty
  packtree pack -dir DIR -out ARCHIVE [-manifest FILE] [-shebang-exec]
  packtree unpack -archive ARCHIVE -dir DIR
  packtree rename -archive ARCHIVE [-manifest FILE]                    apply the product renames inside the archive`)
	os.Exit(2)
}

// Dispatches a verb, a leading flag selecting the pack or repack form the go:generate lines use
func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch verb := os.Args[1]; {
	case verb == "pack":
		err = packVerb(os.Args[2:])
	case verb == "unpack":
		err = unpackVerb(os.Args[2:])
	case verb == "rename":
		err = renameVerb(os.Args[2:])
	case strings.HasPrefix(verb, "-"):
		err = repackVerb(os.Args[1:])
	default:
		usage()
	}
	if err != nil {
		fail(err)
	}
}

// Packs -src into -out, or repacks the existing -out when no source is given
func repackVerb(args []string) error {
	fs := flag.NewFlagSet("packtree", flag.ExitOnError)
	src := fs.String("src", "", "tree to pack, the existing archive being repacked when empty")
	out := fs.String("out", "", "archive to write")
	manifest := fs.String("manifest", "", "manifest to write, none when empty")
	shebangExec := fs.Bool("shebang-exec", false, "pack files that start with #! as executable")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	var files []tarzst.File
	var err error
	if *src != "" {
		files, err = tarzst.Collect(*src, *shebangExec)
	} else {
		files, err = readArchive(*out)
	}
	if err != nil {
		return err
	}
	return writeArchive(*out, *manifest, files)
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

// Rewrites an archive in place with the product renames applied to paths and text bodies
func renameVerb(args []string) error {
	fs := flag.NewFlagSet("rename", flag.ExitOnError)
	archive := fs.String("archive", "", "archive to rewrite")
	manifest := fs.String("manifest", "", "manifest to write, none when empty")
	fs.Parse(args)
	if *archive == "" {
		return fmt.Errorf("rename needs -archive")
	}
	files, err := readArchive(*archive)
	if err != nil {
		return err
	}
	renamed, err := renameArchiveFiles(files)
	if err != nil {
		return err
	}
	return writeArchive(*archive, *manifest, renamed)
}

func readArchive(path string) ([]tarzst.File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return tarzst.Decode(data)
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
