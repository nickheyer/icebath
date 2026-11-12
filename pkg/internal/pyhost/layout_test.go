package pyhost

import (
	"strings"
	"testing"
)

const layoutHeader = `/* leading comment */
typedef struct PyPreConfig {
    int _config_init;     // trailing comment
    int parse_argv;
#ifdef MS_WINDOWS
    int legacy_windows_fs_encoding;
#endif
#if !defined(Py_GIL_DISABLED)
    int utf8_mode;
#else
    int enable_gil;
#endif
    wchar_t *home;
    PyWideStringList argv;
    unsigned long hash_seed;
    size_t n;
} PyPreConfig;
`

// Struct offsets follow C alignment per platform with preprocessor branches chosen by the macros
func TestParseStructOffsets(t *testing.T) {
	cases := []struct {
		goos   string
		macros map[string]bool
		want   map[string]int
		size   int
	}{
		{"linux", nil, map[string]int{"_config_init": 0, "parse_argv": 4, "utf8_mode": 8, "home": 16, "argv": 24, "hash_seed": 40, "n": 48}, 56},
		{"windows", map[string]bool{"MS_WINDOWS": true}, map[string]int{"legacy_windows_fs_encoding": 8, "utf8_mode": 12, "home": 16, "argv": 24, "hash_seed": 40, "n": 48}, 56},
		{"linux", map[string]bool{"Py_GIL_DISABLED": true}, map[string]int{"enable_gil": 8, "home": 16}, 56},
	}
	for _, c := range cases {
		l, err := parseStruct(layoutHeader, "PyPreConfig", c.goos, c.macros)
		if err != nil {
			t.Fatal(err)
		}
		for name, off := range c.want {
			if f, ok := l.Fields[name]; !ok || f.Offset != off {
				t.Errorf("%s %v %s at %d, want %d", c.goos, c.macros, name, f.Offset, off)
			}
		}
		if l.Size != c.size {
			t.Errorf("%s %v size %d, want %d", c.goos, c.macros, l.Size, c.size)
		}
		if l.Fields["home"].Kind != kindWString || l.Fields["argv"].Kind != kindWStringList {
			t.Errorf("kinds %v", l.Fields)
		}
	}
}

// Declarations the parser cannot place refuse instead of guessing an offset
func TestParseStructRefusesUnknownShapes(t *testing.T) {
	cases := map[string]string{
		"typedef struct S {\n    double d;\n} S;\n":                   "unsupported member declaration",
		"typedef struct S {\n#pragma pack\n    int a;\n} S;\n":        "unsupported directive",
		"typedef struct S {\n#if FOO > 1\n    int a;\n#endif\n} S;\n": "unsupported preprocessor condition",
		"typedef struct S {\n    int a;\n":                            "header has no end of S",
		"typedef struct T {\n    int a;\n} T;\n":                      "header has no",
	}
	for src, want := range cases {
		if _, err := parseStruct(src, "S", "linux", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q gave %v, want %q", src, err, want)
		}
	}
}

// The running interpreter's own header parses into a layout that carries every field the init path writes
func TestHostConfigHeaderParses(t *testing.T) {
	h := needHost(t)
	pre, cfg, err := configLayouts(h.Interpreter())
	if err != nil {
		t.Fatal(err)
	}
	for name := range isolatedSentinels {
		if _, ok := cfg.Fields[name]; !ok {
			t.Errorf("PyConfig lacks %s", name)
		}
	}
	for _, name := range []string{"home", "executable", "program_name", "pycache_prefix"} {
		if cfg.Fields[name].Kind != kindWString {
			t.Errorf("PyConfig %s is not a wide string", name)
		}
	}
	if _, ok := pre.Fields["isolated"]; !ok || pre.Size == 0 {
		t.Errorf("PyPreConfig %+v", pre)
	}
}
