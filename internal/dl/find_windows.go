//go:build windows && !386 && !arm

package dl

import (
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// Absolute path of an installed DLL given its short name such as opus, searched where LoadLibrary and PATH look
func Find(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty library name: %w", ErrNotFound)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find %s: %w", name, err)
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("find %s: %w", name, err)
	}
	root, err := windows.GetWindowsDirectory()
	if err != nil {
		return "", fmt.Errorf("find %s: %w", name, err)
	}
	dirs := append([]string{filepath.Dir(exe), system, root}, absDirs(filepath.SplitList(os.Getenv("PATH")))...)
	files := []string{name + ".dll", "lib" + name + ".dll"}
	if strings.HasSuffix(strings.ToLower(name), ".dll") {
		files = []string{name}
	}
	for _, dir := range dirs {
		for _, file := range files {
			if path := filepath.Join(dir, file); loadable(path) {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("%s: %w", name, ErrNotFound)
}

// Whether a file is a DLL this process can map, an x64 process also taking ARM64X hybrids
func loadable(path string) bool {
	f, err := pe.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if f.Characteristics&pe.IMAGE_FILE_DLL == 0 {
		return false
	}
	switch runtime.GOARCH {
	case "amd64":
		return f.Machine == pe.IMAGE_FILE_MACHINE_AMD64 || f.Machine == pe.IMAGE_FILE_MACHINE_ARM64 && hybrid(f)
	case "arm64":
		return f.Machine == pe.IMAGE_FILE_MACHINE_ARM64
	}
	return false
}

// Whether a PE32+ image carries ARM64EC metadata in its load configuration, as ARM64X images do
func hybrid(f *pe.File) bool {
	const chpeOffset = 200
	header, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || header.NumberOfRvaAndSizes <= pe.IMAGE_DIRECTORY_ENTRY_LOAD_CONFIG {
		return false
	}
	dir := header.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_LOAD_CONFIG]
	if dir.Size < chpeOffset+8 {
		return false
	}
	for _, s := range f.Sections {
		if dir.VirtualAddress < s.VirtualAddress || dir.VirtualAddress-s.VirtualAddress >= max(s.VirtualSize, s.Size) {
			continue
		}
		config := make([]byte, chpeOffset+8)
		if _, err := s.ReadAt(config, int64(dir.VirtualAddress-s.VirtualAddress)); err != nil {
			return false
		}
		return binary.LittleEndian.Uint32(config) >= chpeOffset+8 && binary.LittleEndian.Uint64(config[chpeOffset:]) != 0
	}
	return false
}
