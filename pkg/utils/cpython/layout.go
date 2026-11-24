package cpythonutil

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// Member declaration of the config structs, compiled on first use since only the pre-3.14 initialization path parses headers
var memberDecl = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(unsigned\s+long|unsigned\s+int|unsigned|long|int|size_t|Py_ssize_t|wchar_t\s*\*|char\s*\*|PyWideStringList)\s*([A-Za-z_][A-Za-z0-9_]*)\s*;$`)
})

// Header text without C comments: each block comment becomes one space, then each line comment is cut up to its line end
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for {
		i := strings.Index(src, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(src[i+2:], "*/")
		if j < 0 {
			break
		}
		b.WriteString(src[:i])
		b.WriteByte(' ')
		src = src[i+2+j+2:]
	}
	b.WriteString(src)
	text := b.String()
	b.Reset()
	b.Grow(len(text))
	for {
		i := strings.Index(text, "//")
		if i < 0 {
			break
		}
		b.WriteString(text[:i])
		j := strings.IndexByte(text[i:], '\n')
		if j < 0 {
			text = ""
			break
		}
		text = text[i+j:]
	}
	b.WriteString(text)
	return b.String()
}

// Macro an #if condition of the form defined(NAME), defined NAME or their negation tests, and whether it is negated
func parseDefined(expr string) (macro string, negated, ok bool) {
	rest, negated := strings.CutPrefix(expr, "!")
	if rest, ok = strings.CutPrefix(rest, "defined"); !ok {
		return "", false, false
	}
	rest = strings.TrimSuffix(strings.TrimPrefix(rest, "("), ")")
	if rest == "" {
		return "", false, false
	}
	for _, c := range []byte(rest) {
		if c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return "", false, false
		}
	}
	return rest, negated, true
}

// Parses typedef struct name from comment free header text with the given macros defined
func parseStruct(src, name, goos string, macros map[string]bool) (*structLayout, error) {
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
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			directive := strings.Fields(strings.TrimPrefix(line, "#"))
			if len(directive) == 0 {
				continue
			}
			switch directive[0] {
			case "ifdef", "ifndef":
				if len(directive) < 2 {
					return nil, fmt.Errorf("%s: %q names no macro", name, line)
				}
				active = append(active, macros[directive[1]] == (directive[0] == "ifdef"))
			case "if":
				macro, negated, ok := parseDefined(strings.Join(directive[1:], ""))
				if !ok {
					return nil, fmt.Errorf("%s: unsupported preprocessor condition %q", name, line)
				}
				active = append(active, macros[macro] != negated)
			case "else", "endif":
				if len(active) == 0 {
					return nil, fmt.Errorf("%s: %q without an open conditional", name, line)
				}
				if directive[0] == "else" {
					active[len(active)-1] = !active[len(active)-1]
				} else {
					active = active[:len(active)-1]
				}
			default:
				return nil, fmt.Errorf("%s: unsupported directive %q", name, line)
			}
			continue
		}
		if !enabled() {
			continue
		}
		m := memberDecl().FindStringSubmatch(strings.Join(strings.Fields(line), " "))
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
		maxAlign = max(maxAlign, align)
	}
	if len(active) != 0 {
		return nil, fmt.Errorf("%s: unbalanced preprocessor conditionals", name)
	}
	layout.Size = (offset + maxAlign - 1) / maxAlign * maxAlign
	return layout, nil
}

// Layouts of PyPreConfig and PyConfig for the interpreter, read from cpython/initconfig.h
func configLayouts(in *Interpreter) (pre, cfg *structLayout, err error) {
	data, err := os.ReadFile(filepath.Join(in.IncludeDir, "cpython", "initconfig.h"))
	if err != nil {
		return nil, nil, fmt.Errorf("PyConfig layout needs the interpreter headers: %w", err)
	}
	src := stripComments(string(data))
	if pre, err = parseStruct(src, "PyPreConfig", in.GOOS, in.Macros); err != nil {
		return nil, nil, err
	}
	if cfg, err = parseStruct(src, "PyConfig", in.GOOS, in.Macros); err != nil {
		return nil, nil, err
	}
	return pre, cfg, nil
}
