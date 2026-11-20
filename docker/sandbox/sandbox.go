// Package sandbox carries the Dockerfile of the image icebath builds for its container sandboxes
package sandbox

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
)

//go:embed Dockerfile
var Dockerfile string

// Image label holding the Dockerfile digest a build came from
const DigestLabel = "app.icebath.sandbox.dockerfile"

// Hex sha256 of the embedded Dockerfile
func Digest() string {
	sum := sha256.Sum256([]byte(Dockerfile))
	return hex.EncodeToString(sum[:])
}

// Instruction keyword and its arguments, continuation lines joined
type Instruction struct {
	Keyword string
	Args    string
}

// Instructions of the embedded Dockerfile in order, comments and blank lines dropped
func Instructions() ([]Instruction, error) {
	var out []Instruction
	var pending strings.Builder
	for _, raw := range strings.Split(Dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if pending.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			pending.WriteString(strings.TrimSuffix(line, "\\"))
			pending.WriteString(" ")
			continue
		}
		pending.WriteString(line)
		keyword, args, _ := strings.Cut(pending.String(), " ")
		out = append(out, Instruction{Keyword: strings.ToUpper(keyword), Args: strings.TrimSpace(args)})
		pending.Reset()
	}
	if pending.Len() > 0 {
		return nil, fmt.Errorf("sandbox Dockerfile ends inside a continued line")
	}
	if len(out) == 0 || out[0].Keyword != "FROM" {
		return nil, fmt.Errorf("sandbox Dockerfile does not start with FROM")
	}
	return out, nil
}
