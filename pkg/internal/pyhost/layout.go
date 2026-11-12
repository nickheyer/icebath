package pyhost

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

type fieldKind int

const (
	kindInt fieldKind = iota
	kindULong
	kindSize
	kindWString
	kindWStringList
)

type structField struct {
	Offset int
	Kind   fieldKind
}

// Byte offsets of a C struct computed from the interpreter's own header for one platform
type structLayout struct {
	Name   string
	Fields map[string]structField
	Size   int
}

var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`//[^\n]*`)
	declRe       = regexp.MustCompile(`^(unsigned\s+long|unsigned\s+int|unsigned|long|int|size_t|Py_ssize_t|wchar_t\s*\*|char\s*\*|PyWideStringList)\s*([A-Za-z_][A-Za-z0-9_]*)\s*;$`)
	definedRe    = regexp.MustCompile(`^(!?)defined\(?([A-Za-z_0-9]+)\)?$`)
)

// Parses typedef struct name from header text with the given macros defined
func parseStruct(src, name, goos string, macros map[string]bool) (*structLayout, error) {
	src = blockComment.ReplaceAllString(src, " ")
	src = lineComment.ReplaceAllString(src, "")
	open := "typedef struct " + name + " {"
	start := strings.Index(src, open)
	if start < 0 {
		return nil, fmt.Errorf("header has no %q", open)
	}
	body := src[start+len(open):]
	end := strings.Index(body, "} "+name+";")
	if end < 0 {
		return nil, fmt.Errorf("header has no end of %s", name)
	}
	body = body[:end]
	ulong := 8
	if goos == "windows" {
		ulong = 4
	}
	layout := &structLayout{Name: name, Fields: map[string]structField{}}
	var active []bool
	enabled := func() bool {
		for _, a := range active {
			if !a {
				return false
			}
		}
		return true
	}
	offset, maxAlign := 0, 1
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			directive := strings.Fields(strings.TrimPrefix(line, "#"))
			switch directive[0] {
			case "ifdef":
				active = append(active, macros[directive[1]])
			case "ifndef":
				active = append(active, !macros[directive[1]])
			case "if":
				expr := strings.Join(directive[1:], "")
				if m := definedRe.FindStringSubmatch(expr); m != nil {
					active = append(active, macros[m[2]] != (m[1] == "!"))
				} else {
					return nil, fmt.Errorf("%s: unsupported preprocessor condition %q", name, line)
				}
			case "else":
				active[len(active)-1] = !active[len(active)-1]
			case "endif":
				active = active[:len(active)-1]
			default:
				return nil, fmt.Errorf("%s: unsupported directive %q", name, line)
			}
			continue
		}
		if !enabled() {
			continue
		}
		m := declRe.FindStringSubmatch(strings.Join(strings.Fields(line), " "))
		if m == nil {
			return nil, fmt.Errorf("%s: unsupported member declaration %q", name, line)
		}
		typ := strings.Join(strings.Fields(m[1]), " ")
		var size, align int
		var kind fieldKind
		switch typ {
		case "int", "unsigned int", "unsigned":
			size, align, kind = 4, 4, kindInt
		case "unsigned long", "long":
			size, align, kind = ulong, ulong, kindULong
		case "size_t", "Py_ssize_t":
			size, align, kind = 8, 8, kindSize
		case "wchar_t *", "wchar_t*", "char *", "char*":
			size, align, kind = 8, 8, kindWString
		case "PyWideStringList":
			size, align, kind = 16, 8, kindWStringList
		default:
			return nil, fmt.Errorf("%s: unknown member type %q", name, typ)
		}
		offset = (offset + align - 1) / align * align
		layout.Fields[m[2]] = structField{Offset: offset, Kind: kind}
		offset += size
		if align > maxAlign {
			maxAlign = align
		}
	}
	if len(active) != 0 {
		return nil, fmt.Errorf("%s: unbalanced preprocessor conditionals", name)
	}
	layout.Size = (offset + maxAlign - 1) / maxAlign * maxAlign
	return layout, nil
}

// Layouts of PyPreConfig and PyConfig for the interpreter, read from cpython/initconfig.h
func configLayouts(in *Interpreter) (pre, cfg *structLayout, err error) {
	path := in.IncludeDir + string(os.PathSeparator) + "cpython" + string(os.PathSeparator) + "initconfig.h"
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("PyConfig layout needs the interpreter headers: %w", err)
	}
	if pre, err = parseStruct(string(data), "PyPreConfig", in.GOOS, in.Macros); err != nil {
		return nil, nil, err
	}
	if cfg, err = parseStruct(string(data), "PyConfig", in.GOOS, in.Macros); err != nil {
		return nil, nil, err
	}
	return pre, cfg, nil
}
