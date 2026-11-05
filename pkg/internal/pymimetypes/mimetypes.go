package pymimetypes

import (
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// KnownFiles are the mime.types files the module database reads after the Windows registry
var KnownFiles = []string{
	"/etc/mime.types",
	"/etc/httpd/mime.types",
	"/etc/httpd/conf/mime.types",
	"/etc/apache/mime.types",
	"/etc/apache2/mime.types",
	"/usr/local/etc/httpd/conf/mime.types",
	"/usr/local/lib/netscape/mime.types",
	"/usr/local/etc/httpd/conf/mime.types",
	"/usr/local/etc/mime.types",
}

// DB holds the suffix, encoding and per strictness type tables of a MimeTypes instance
type DB struct {
	encodings map[string]string
	suffixes  map[string]string
	types     [2]map[string]string
}

// New builds a database from the CPython default tables only
func New() *DB {
	d := &DB{encodings: map[string]string{}, suffixes: map[string]string{}, types: [2]map[string]string{{}, {}}}
	for _, e := range encodingsMapDefault {
		d.encodings[e.k] = e.v
	}
	for _, e := range suffixMapDefault {
		d.suffixes[e.k] = e.v
	}
	for _, e := range typesMapDefault {
		d.AddType(e.v, e.k, true)
	}
	for _, e := range commonTypesDefault {
		d.AddType(e.v, e.k, false)
	}
	return d
}

// AddType maps an extension to a type in the strict or non strict table, ignoring an empty type
func (d *DB) AddType(typ, ext string, strict bool) {
	if typ == "" {
		return
	}
	d.types[strictIndex(strict)][ext] = typ
}

func strictIndex(strict bool) int {
	if strict {
		return 1
	}
	return 0
}

// Read loads a mime.types file into the database, returning the open or read error
func (d *DB) Read(filename string, strict bool) error {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	d.ReadText(pystr.SurrogateEscapeDecode(raw), strict)
	return nil
}

// ReadText parses mime.types lines with universal newlines, a word starting with a hash ending the line
func (d *DB) ReadText(text string, strict bool) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.SplitAfter(text, "\n") {
		words := pystr.SplitWhitespace(line, -1)
		for i, w := range words {
			if strings.HasPrefix(w, "#") {
				words = words[:i]
				break
			}
		}
		if len(words) == 0 {
			continue
		}
		for _, suff := range words[1:] {
			d.AddType(words[0], "."+suff, strict)
		}
	}
}

var (
	globalOnce sync.Once
	globalDB   *DB
)

// Default is the module database, defaults then the Windows registry then every existing known file
func Default() *DB {
	globalOnce.Do(func() {
		d := New()
		readWindowsRegistry(d)
		for _, f := range KnownFiles {
			if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
				d.Read(f, true)
			}
		}
		globalDB = d
	})
	return globalDB
}

// GuessType is mimetypes.guess_type on the module database, empty strings standing for None
func GuessType(url string, strict bool) (typ, encoding string, err error) {
	return Default().GuessType(url, strict)
}

// GuessFileType is mimetypes.guess_file_type on the module database
func GuessFileType(path string, strict bool) (typ, encoding string) {
	return Default().GuessFileType(path, strict)
}

// GuessType parses the URL, answering data URLs from their header and other paths from the tables
func (d *DB) GuessType(url string, strict bool) (string, string, error) {
	p, err := pyurl.Parse(url)
	if err != nil {
		return "", "", err
	}
	if len(p.Scheme) <= 1 {
		typ, enc := d.GuessFileType(url, strict)
		return typ, enc, nil
	}
	if p.Scheme == "data" {
		comma := strings.IndexByte(p.Path, ',')
		if comma < 0 {
			return "", "", nil
		}
		typ := p.Path[:comma]
		if semi := strings.IndexByte(typ, ';'); semi >= 0 {
			typ = typ[:semi]
		}
		if strings.Contains(typ, "=") || !strings.Contains(typ, "/") {
			typ = "text/plain"
		}
		return typ, "", nil
	}
	typ, enc := d.guessFileType(p.Path, strict, pypath.PosixSplitext)
	return typ, enc, nil
}

// GuessFileType drops the drive and guesses from the host path suffixes
func (d *DB) GuessFileType(path string, strict bool) (string, string) {
	if hostWindows {
		_, path = pypath.NtSplitdrive(path)
	}
	return d.guessFileType(path, strict, pypath.Splitext)
}

func (d *DB) guessFileType(path string, strict bool, splitext func(string) (string, string)) (string, string) {
	base, ext := splitext(path)
	for {
		repl, ok := d.suffixes[pystr.Lower(ext)]
		if !ok {
			break
		}
		base, ext = splitext(base + repl)
	}
	encoding := ""
	if enc, ok := d.encodings[ext]; ok {
		encoding = enc
		base, ext = splitext(base)
	}
	ext = pystr.Lower(ext)
	if typ, ok := d.types[1][ext]; ok {
		return typ, encoding
	}
	if strict {
		return "", encoding
	}
	return d.types[0][ext], encoding
}
