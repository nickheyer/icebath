package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Spans that keep upstream spelling, URLs, repository and image slugs, upstream image paths, Nous model names and author attributions
var keptSpans = regexp.MustCompile(`https?://[^\s"'<>)\]` + "`" + `]+` +
	`|\b(?i:nousresearch|purzbeats)/[A-Za-z0-9_.:-]+` +
	`|/opt/hermes/` +
	`|\bNous-Hermes[A-Za-z0-9_.-]*` +
	`|\bHermes 4\b|\bHermes/Grok\b|\(Hermes\) \x{2014} uncensored` +
	`|(?m:^author:.*$)|# MIT License\. Part of the Hermes [a-z]+ skill\.`)

// Possessive of the product name, whose new spelling takes an apostrophe s
var possessive = regexp.MustCompile(`(^|[^'"\w])Hermes'( )`)

// Product renames applied outside kept spans in order, CLI, home directory, environment prefix, module and product names
var productRenames = []struct{ from, to string }{
	{"HERMES", "ICEBATH"},
	{"Hermes", "Icebath"},
	{"hermes", "icebath"},
}

// Renamed texts reworded to fit the 60 character skill index that every shipped skill description stays within
var fittedTexts = []struct{ from, to string }{
	{"Use, configure, theme, extend, and orchestrate Icebath Agent.", "Use, configure, theme, extend and orchestrate Icebath Agent."},
}

// Applies the product renames to text outside every kept span, then the fitted rewordings
func renameText(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range keptSpans.FindAllStringIndex(s, -1) {
		b.WriteString(renameSegment(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(renameSegment(s[last:]))
	out := b.String()
	for _, f := range fittedTexts {
		out = strings.ReplaceAll(out, f.from, f.to)
	}
	return out
}

func renameSegment(s string) string {
	s = possessive.ReplaceAllString(s, "${1}Icebath's${2}")
	for _, r := range productRenames {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	return s
}

// Text files carry valid UTF-8 and no NUL byte, everything else keeps its bytes
func isText(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// Renames every path and text body keeping modes and binary bodies, refusing two paths that rename alike
func renameArchiveFiles(files []tarzst.File) ([]tarzst.File, error) {
	out := make([]tarzst.File, 0, len(files))
	from := map[string]string{}
	for _, f := range files {
		data := f.Data
		if isText(data) {
			data = []byte(renameText(string(data)))
		}
		path := renameText(f.Path)
		if prev, dup := from[path]; dup {
			return nil, fmt.Errorf("%s and %s both rename to %s", prev, f.Path, path)
		}
		from[path] = f.Path
		out = append(out, tarzst.NewFile(path, data, f.Mode))
	}
	return out, nil
}
