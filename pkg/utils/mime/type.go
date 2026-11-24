package mimeutil

import (
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Media type of a file extension given with its leading dot, matched without regard to case: the built-in table's type for every extension it holds, else the host's through the standard mime package (the shared MIME database or mime.types files on Unix, the registry on Windows) with parameters such as charset cut, "" when neither knows it
func TypeByExtension(ext string) string {
	if ext == "" {
		return ""
	}
	if typ, ok := lookup().byExt[strings.ToLower(ext)]; ok {
		return typ
	}
	return essence(mime.TypeByExtension(ext))
}

// Media type of a file name or path from the extension of its last element as TypeByExtension types it, "" when it has no known one; a leading dot starts no extension, so .png is a name, and a compressed file is typed by its compression (x.tar.gz is application/gzip)
func TypeByName(name string) string {
	name = name[len(filepath.VolumeName(name)):]
	i := len(name) - 1
	for i >= 0 && !os.IsPathSeparator(name[i]) {
		i--
	}
	return TypeByExtension(extension(name[i+1:]))
}

// Media type of a URL, the declared type of a data URL (text/plain when it declares none or an invalid one, "" without its comma), else the type of the URL path's last segment with any query, fragment and ;parameters cut; text with no scheme of two or more characters is a file name or path as TypeByName reads it
func TypeByURL(ref string) string {
	scheme, rest, ok := cutScheme(ref)
	if !ok {
		return TypeByName(ref)
	}
	if strings.EqualFold(scheme, "data") {
		return dataURLType(rest)
	}
	if after, found := strings.CutPrefix(rest, "//"); found {
		i := strings.IndexAny(after, "/?#")
		if i < 0 || after[i] != '/' {
			return ""
		}
		rest = after[i:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	segment := rest[strings.LastIndexByte(rest, '/')+1:]
	if i := strings.IndexByte(segment, ';'); i >= 0 {
		segment = segment[:i]
	}
	if decoded, err := url.PathUnescape(segment); err == nil {
		segment = decoded
	}
	return TypeByExtension(extension(segment))
}

// Whether a media type, parameters ignored, carries text: any text type, any +json, +xml or +yaml type, the textual application types (JSON, XML, YAML, TOML, JavaScript, SQL, shell, PHP, TeX, troff, Tcl, RDF, HLS playlist, MIF, WAIS source) and mail messages
func IsText(typ string) bool {
	typ = essence(typ)
	switch {
	case strings.HasPrefix(typ, "text/"), strings.HasSuffix(typ, "+json"), strings.HasSuffix(typ, "+xml"), strings.HasSuffix(typ, "+yaml"):
		return true
	}
	switch typ {
	case "application/json", "application/x-ndjson", "application/xml", "application/yaml", "application/x-yaml", "application/toml",
		"application/javascript", "application/x-javascript", "application/ecmascript", "application/sql",
		"application/x-sh", "application/x-csh", "application/x-shar", "application/x-httpd-php",
		"application/x-tex", "application/x-latex", "application/x-texinfo", "application/x-mif",
		"application/x-troff", "application/x-troff-man", "application/x-troff-me", "application/x-troff-ms",
		"application/x-tcl", "application/x-wais-source", "application/n-quads", "application/n-triples", "application/trig",
		"application/vnd.apple.mpegurl", "message/rfc822":
		return true
	}
	return false
}

// Extension of a final path element from its last dot, "" when the dots before it are all the element has
func extension(base string) string {
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 || strings.TrimLeft(base[:dot], ".") == "" {
		return ""
	}
	return base[dot:]
}

// URL scheme of the text and what follows its colon, false unless the text starts with a letter and two or more scheme characters before a colon
func cutScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		case c == ':' && i >= 2:
			return s[:i], s[i+1:], true
		default:
			return "", "", false
		}
	}
	return "", "", false
}

// Declared type of the data URL text after data:, lower case without parameters
func dataURLType(rest string) string {
	header, _, found := strings.Cut(rest, ",")
	if !found {
		return ""
	}
	if typ := essence(header); validType(typ) {
		return typ
	}
	return "text/plain"
}

// Type and subtype of a media type, lower case, parameters and surrounding space cut
func essence(typ string) string {
	if i := strings.IndexByte(typ, ';'); i >= 0 {
		typ = typ[:i]
	}
	return strings.ToLower(strings.TrimSpace(typ))
}

// Whether the text is one type/subtype pair of token characters
func validType(typ string) bool {
	major, minor, found := strings.Cut(typ, "/")
	return found && isToken(major) && isToken(minor)
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?={}`, c) >= 0 {
			return false
		}
	}
	return true
}
