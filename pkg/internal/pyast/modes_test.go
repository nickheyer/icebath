package pyast

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type modeCase struct {
	Name     string  `json:"name"`
	Src      string  `json:"src"`
	Mode     string  `json:"mode"`
	TC       bool    `json:"tc"`
	FV       *int    `json:"fv"`
	Opt      int     `json:"opt"`
	WError   bool    `json:"werror"`
	Dump     *string `json:"dump"`
	Unparse  *string `json:"unparse"`
	Err      []any   `json:"err"`
	Warnings [][]any `json:"warnings"`
	Hex      string  `json:"hex"`
	FileHex  string  `json:"filehex"`
	FileErr  []any   `json:"fileerr"`
}

// Parses a recorded case with the source file the case names written to a temporary path
func parseModeCase(t *testing.T, c modeCase) (*Node, []Warning, error) {
	opts := Options{Mode: c.Mode, TypeComments: c.TC, FeatureVersion: c.FV, Optimize: c.Opt, WarningsAsErrors: c.WError}
	if c.FileHex != "" {
		content, _ := hex.DecodeString(c.FileHex)
		opts.Filename = filepath.Join(t.TempDir(), "srcfile.py")
		if err := os.WriteFile(opts.Filename, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c.Hex != "" {
		src, _ := hex.DecodeString(c.Hex)
		return ParseBytesWith(src, opts)
	}
	return ParseWith(c.Src, opts)
}

func warningRows(ws []Warning) string {
	rows := make([]string, len(ws))
	for i, w := range ws {
		rows[i] = fmt.Sprintf("%s|%s|%s|%d", w.Category, w.Msg, w.Filename, w.Lineno)
	}
	return strings.Join(rows, "\n")
}

func TestParseModesMatchCPython(t *testing.T) {
	var cases []modeCase
	if err := json.Unmarshal(readGzip(t, "modes.json.gz"), &cases); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, c := range cases {
		n, ws, err := parseModeCase(t, c)
		var got, want string
		if c.FileHex != "" {
			var se *SyntaxError
			if c.FileErr == nil || !errors.As(err, &se) {
				failed++
				t.Errorf("%s got %v want %v", c.Name, err, c.FileErr)
				continue
			}
			got = errorRow(err) + "|" + se.Text
			want = expectedErrorRow(c.FileErr[:6]) + "|" + strings.TrimSuffix(c.FileErr[6].(string), "\n")
			if got != want {
				failed++
				t.Errorf("%s\n got %q\nwant %q", c.Name, got, want)
			}
			continue
		}
		if c.Err != nil {
			want = "err:" + expectedErrorRow(c.Err)
		} else {
			want = "dump:" + *c.Dump
		}
		if err != nil {
			got = "err:" + errorRow(err)
		} else if d, derr := Dump(n, true); derr != nil {
			got = "dumperr:" + derr.Error()
		} else {
			got = "dump:" + d
		}
		if got != want {
			failed++
			t.Errorf("%s %q\n got %s\nwant %s", c.Name, firstRunes(c.Src, 80), firstRunes(got, 600), firstRunes(want, 600))
			continue
		}
		if c.Unparse != nil {
			if up, uerr := Unparse(n); uerr != nil || up != *c.Unparse {
				failed++
				t.Errorf("%s unparse\n got %q %v\nwant %q", c.Name, up, uerr, *c.Unparse)
			}
		}
		wantRows := make([]string, len(c.Warnings))
		for i, w := range c.Warnings {
			wantRows[i] = fmt.Sprintf("%v|%v|%v|%d", w[0], w[1], w[2], int(w[3].(float64)))
		}
		if g, w := warningRows(ws), strings.Join(wantRows, "\n"); g != w {
			failed++
			t.Errorf("%s %q warnings\n got %s\nwant %s", c.Name, firstRunes(c.Src, 80), g, w)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d mode cases differ from CPython", failed, len(cases))
	}
}
