package pyast

import (
	"encoding/hex"
	"strings"
)

type compileCase struct {
	Name string  `json:"name"`
	Src  *string `json:"src"`
	Hex  string  `json:"hex"`
	Out  any     `json:"out"`
	FOut any     `json:"fout"`
}

func (c compileCase) compile(filename string) error {
	if c.Src != nil {
		return CompileString(*c.Src, filename)
	}
	src, _ := hex.DecodeString(c.Hex)
	return Compile(src, filename)
}

func (c compileCase) bytes() []byte {
	if c.Src != nil {
		return []byte(*c.Src)
	}
	src, _ := hex.DecodeString(c.Hex)
	return src
}

// Builds the chain of n units and places it at the marker of a template
func (r recursionRow) sourceIn(tmpl string, n int) string {
	return strings.Replace(tmpl, "\x01", r.source(n), 1)
}
