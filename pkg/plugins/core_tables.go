package plugins

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Core module facts the source audits read, derived from the hermes reference tree and the plugin SDK
//
//go:embed core_tables.txt.gz
var coreTablesGz []byte

type coreTableSet struct {
	topLevel map[string]bool
	packages map[string]bool
	private  map[string]bool
	tree     map[string]byte
}

var coreTables = sync.OnceValue(func() *coreTableSet {
	zr, err := gzip.NewReader(bytes.NewReader(coreTablesGz))
	if err != nil {
		panic("plugins: embedded core tables are not gzip: " + err.Error())
	}
	t := &coreTableSet{topLevel: map[string]bool{}, packages: map[string]bool{}, private: map[string]bool{}, tree: map[string]byte{}}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		kind, value, _ := strings.Cut(sc.Text(), " ")
		switch kind {
		case "top":
			t.topLevel[value] = true
		case "pkg":
			t.packages[value] = true
		case "priv":
			t.private[value] = true
		case "d", "f":
			t.tree[coreTreeKey(value)] = kind[0]
		}
	}
	if err := sc.Err(); err != nil {
		panic("plugins: embedded core tables are unreadable: " + err.Error())
	}
	return t
})

// Case folded on the filesystems Python stats case insensitively
func coreTreeKey(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// Whether root joined with the dotted parts names a core .py file or directory, as pathlib stats it
func coreModuleExists(module string) bool {
	base, rooted, segs := "", false, []string(nil)
	for _, part := range strings.Split(module, ".") {
		vol := filepath.VolumeName(part)
		if vol != "" || (part != "" && coreIsPathSep(rune(part[0]))) {
			part = part[len(vol):]
			base, rooted, segs = vol, part != "" && coreIsPathSep(rune(part[0])), nil
			if vol == "" {
				rooted = true
			}
		}
		for _, p := range strings.FieldsFunc(part, coreIsPathSep) {
			if p != "." {
				segs = append(segs, p)
			}
		}
	}
	if base != "" || rooted {
		if rooted {
			base += string(filepath.Separator)
		}
		p := base + strings.Join(segs, string(filepath.Separator))
		if len(segs) > 0 {
			if fi, err := os.Stat(p + ".py"); err == nil && fi.Mode().IsRegular() {
				return true
			}
		}
		fi, err := os.Stat(p)
		return err == nil && fi.IsDir()
	}
	if len(segs) > 0 {
		file := append(append([]string{}, segs[:len(segs)-1]...), segs[len(segs)-1]+".py")
		if coreTreeWalk(file) == 'f' {
			return true
		}
	}
	return coreTreeWalk(segs) == 'd'
}

func coreIsPathSep(r rune) bool { return r == '/' || (runtime.GOOS == "windows" && r == '\\') }

// Kind of the entry a relative component list reaches inside the core tree, each parent required to be a directory
func coreTreeWalk(segs []string) byte {
	t := coreTables().tree
	stack := []string{}
	kind := byte('d')
	for _, s := range segs {
		if kind != 'd' {
			return 0
		}
		if s == ".." {
			if len(stack) == 0 {
				return 0
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, s)
		}
		if len(stack) == 0 {
			kind = 'd'
			continue
		}
		kind = t[coreTreeKey(strings.Join(stack, "/"))]
		if kind == 0 {
			return 0
		}
	}
	return kind
}
