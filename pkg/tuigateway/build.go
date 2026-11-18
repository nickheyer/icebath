package tuigateway

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
)

// Version of the wire contract, raised when a declared type changes so that a peer of an older contract misreads it
const ContractVersion = 1

// Go build ID of the running executable, empty when the executable cannot be read
var BuildID = sync.OnceValue(readBuildID)

// Read at start so a binary replaced on disk later never changes the answer
func init() { go BuildID() }

// Whether a peer that reported this build and contract version runs the same build as this process
func SameBuild(build string, contract int64) bool {
	return build == BuildID() && contract == ContractVersion
}

const (
	buildIDScan     = 32 << 10
	elfGoBuildIDTag = 4
)

var (
	goBuildPrefix = []byte("\xff Go build ID: \"")
	goBuildEnd    = []byte("\"\n \xff")
	elfGoNote     = []byte("Go\x00\x00")
)

// Build ID the Go linker stamped into the image this process started from
func readBuildID() string {
	path := "/proc/self/exe"
	if runtime.GOOS != "linux" {
		exe, err := os.Executable()
		if err != nil {
			return ""
		}
		path = exe
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, buildIDScan)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if bytes.HasPrefix(head, []byte("\x7fELF")) {
		return elfBuildID(f)
	}
	if id := machoBuildID(f); id != "" {
		return id
	}
	return rawBuildID(head)
}

// Build ID of an ELF executable, the Go note of its note segments
func elfBuildID(f *os.File) string {
	ef, err := elf.NewFile(f)
	if err != nil {
		return ""
	}
	for _, p := range ef.Progs {
		if p.Type != elf.PT_NOTE || p.Filesz < 16 {
			continue
		}
		note := make([]byte, min(p.Filesz, 1<<16))
		if _, err := p.ReadAt(note, 0); err != nil {
			continue
		}
		for len(note) >= 16 {
			nameSize := uint64(ef.ByteOrder.Uint32(note))
			valSize := uint64(ef.ByteOrder.Uint32(note[4:]))
			tag := ef.ByteOrder.Uint32(note[8:])
			descAt := 12 + (nameSize+3)&^3
			if descAt+valSize > uint64(len(note)) {
				break
			}
			if tag == elfGoBuildIDTag && nameSize == 4 && bytes.Equal(note[12:16], elfGoNote) {
				return string(note[descAt : descAt+valSize])
			}
			next := descAt + (valSize+3)&^3
			if next >= uint64(len(note)) {
				break
			}
			note = note[next:]
		}
	}
	return ""
}

// Build ID of a Mach-O executable, stamped at the start of its text section, empty for any other format
func machoBuildID(f *os.File) string {
	mf, err := macho.NewFile(f)
	if err != nil {
		return ""
	}
	sect := mf.Section("__text")
	if sect == nil {
		return ""
	}
	buf := make([]byte, min(sect.Size, buildIDScan))
	if _, err := f.ReadAt(buf, int64(sect.Offset)); err != nil {
		return ""
	}
	return rawBuildID(buf)
}

// Build ID stamped as text between the Go markers, as PE and other formats carry it near the file start
func rawBuildID(data []byte) string {
	i := bytes.Index(data, goBuildPrefix)
	if i < 0 {
		return ""
	}
	rest := data[i+len(goBuildPrefix):]
	j := bytes.Index(rest, goBuildEnd)
	if j < 0 {
		return ""
	}
	id, err := strconv.Unquote(`"` + string(rest[:j]) + `"`)
	if err != nil {
		return ""
	}
	return id
}
