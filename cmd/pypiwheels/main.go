package main

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"

	"archive/zip"
)

const name = "icebath_cli"

const summary = "Tiling agentic harness and netrunner toolkit"

const description = "# icebath\n\nA tiling agentic harness and netrunner toolkit for a deep dive beyond the blackwall.\n\n" +
	"```sh\npipx install icebath-cli\nicebath\n```\n\nSee https://icebath.app for every other way to install it.\n"

// One release binary target and the wheel platform tag it ships under, in publishing order
type platform struct {
	goos, goarch, tag string
}

var platforms = []platform{
	{"linux", "amd64", "manylinux_2_17_x86_64.manylinux2014_x86_64.musllinux_1_1_x86_64"},
	{"linux", "arm64", "manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64"},
	{"darwin", "amd64", "macosx_13_0_x86_64"},
	{"darwin", "arm64", "macosx_13_0_arm64"},
	{"windows", "amd64", "win_amd64"},
	{"windows", "arm64", "win_arm64"},
}

// One archive member with its bytes and permission bits
type member struct {
	name string
	data []byte
	mode uint32
}

// The sha256 RECORD digest of some bytes, urlsafe base64 without padding
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256=" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// METADATA text of the icebath-cli distribution at version
func metadata(version string) string {
	return strings.Join([]string{
		"Metadata-Version: 2.4",
		"Name: icebath-cli",
		"Version: " + version,
		"Summary: " + summary,
		"Author-email: Nicholas W. Heyer <nick@heyer.app>",
		"License-Expression: MIT",
		"License-File: LICENSE",
		"Project-URL: Homepage, https://icebath.app",
		"Project-URL: Source, https://github.com/nickheyer/icebath",
		"Description-Content-Type: text/markdown",
		"",
		description,
	}, "\n")
}

// WHEEL text naming one py3-none tag per dotted platform tag
func wheelFile(tag string) string {
	lines := []string{"Wheel-Version: 1.0", "Generator: icebath", "Root-Is-Purelib: false"}
	for _, t := range strings.Split(tag, ".") {
		lines = append(lines, "Tag: py3-none-"+t)
	}
	return strings.Join(append(lines, ""), "\n")
}

// Members of one wheel in archive order, RECORD last listing every other member
func members(version string, binary []byte, exe, tag string, license []byte) []member {
	info := fmt.Sprintf("%s-%s.dist-info", name, version)
	files := []member{
		{fmt.Sprintf("%s-%s.data/scripts/icebath%s", name, version, exe), binary, 0o755},
		{fmt.Sprintf("%s-%s.data/scripts/.install_method", name, version), []byte("pip\n"), 0o644},
		{info + "/METADATA", []byte(metadata(version)), 0o644},
		{info + "/WHEEL", []byte(wheelFile(tag)), 0o644},
		{info + "/licenses/LICENSE", license, 0o644},
	}
	var record strings.Builder
	for _, f := range files {
		fmt.Fprintf(&record, "%s,%s,%d\n", f.name, digest(f.data), len(f.data))
	}
	record.WriteString(info + "/RECORD,,\n")
	return append(files, member{info + "/RECORD", []byte(record.String()), 0o644})
}

// Writes one deflated member with known sizes, a 1980-01-01 stamp and Unix permission bits
func writeMember(zw *zip.Writer, m member) error {
	var compressed bytes.Buffer
	fw, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		return err
	}
	if _, err := fw.Write(m.data); err != nil {
		return err
	}
	if err := fw.Close(); err != nil {
		return err
	}
	header := &zip.FileHeader{
		Name:               m.name,
		Method:             zip.Deflate,
		CreatorVersion:     3<<8 | 20,
		ReaderVersion:      20,
		ModifiedDate:       1<<5 | 1,
		CRC32:              crc32.ChecksumIEEE(m.data),
		CompressedSize64:   uint64(compressed.Len()),
		UncompressedSize64: uint64(len(m.data)),
		ExternalAttrs:      (0o100000 | m.mode) << 16,
	}
	w, err := zw.CreateRaw(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, &compressed)
	return err
}

// Writes the wheel for one platform into out and returns its path
func wheel(version string, binary []byte, exe, tag string, license []byte, out string) (string, error) {
	path := filepath.Join(out, fmt.Sprintf("%s-%s-py3-none-%s.whl", name, version, tag))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	zw := zip.NewWriter(f)
	for _, m := range members(version, binary, exe, tag, license) {
		if err := writeMember(zw, m); err != nil {
			f.Close()
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

// Writes a wheel for every platform from release binaries named icebath-os-arch, printing each path
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 4 {
		fmt.Fprintln(stderr, "usage: pypiwheels <version> <binaries dir> <out dir> <license file>")
		return 1
	}
	version, binaries, out, licensePath := args[0], args[1], args[2], args[3]
	if err := os.MkdirAll(out, 0o777); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	license, err := os.ReadFile(licensePath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, p := range platforms {
		exe := ""
		if p.goos == "windows" {
			exe = ".exe"
		}
		binaryPath := filepath.Join(binaries, fmt.Sprintf("icebath-%s-%s%s", p.goos, p.goarch, exe))
		if st, err := os.Stat(binaryPath); err != nil || !st.Mode().IsRegular() {
			fmt.Fprintf(stderr, "wheels: missing %s\n", binaryPath)
			return 1
		}
		binary, err := os.ReadFile(binaryPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		path, err := wheel(version, binary, exe, p.tag, license, out)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, path)
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
