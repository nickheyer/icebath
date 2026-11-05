package pm

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// TermuxPool is the Termux main package pool the bionic pins come from
const TermuxPool = "https://packages.termux.dev/apt/termux-main/"

// TermuxIndexURL is the pool's aarch64 package index
const TermuxIndexURL = TermuxPool + "dists/stable/main/binary-aarch64/Packages"

const termuxPoolPathPrefix = "/apt/termux-main/pool/main/"

// TermuxTableName is the runtime library pin table beside this package
const TermuxTableName = "termux_runtime_libs.json"

//go:embed termux_runtime_libs.json
var embeddedTermuxTable []byte

// Host of the Termux pool
var termuxPoolHost = func() string {
	u, _ := url.Parse(TermuxPool)
	return u.Hostname()
}()

// PinMismatchError means the pool serves bytes the package index does not describe
type PinMismatchError struct{ Message string }

func (e *PinMismatchError) Error() string { return e.Message }

// Source path of the pin table, the shipped binary reading its embedded copy
var termuxTablePath = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join(RepoRoot(), "pm", TermuxTableName)
	}
	return filepath.Join(filepath.Dir(file), TermuxTableName)
}

// Path of the runtime library pin table
func TermuxTablePath() string { return termuxTablePath() }

// Loads the pin table keeping its key order, the default path falling back to the embedded copy
func LoadTermuxTable(path string) (*OrderedObject, error) {
	if path == "" {
		path = TermuxTablePath()
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && resolvePath(path) == resolvePath(TermuxTablePath()) {
		data, err = embeddedTermuxTable, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := loadOrderedJSON(data)
	if err != nil {
		return nil, err
	}
	table, ok := v.(*OrderedObject)
	if !ok {
		return nil, errors.New("termux runtime library table is not an object")
	}
	return table, nil
}

// Writes the pin table indented in its key order with a trailing newline
func SaveTermuxTable(table *OrderedObject, path string) error {
	if path == "" {
		path = TermuxTablePath()
	}
	body, err := dumpsJSON(table, true)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o666)
}

// PoolPackage is the archive the pool currently serves for one package
type PoolPackage struct {
	Name    string
	Version string
	URL     string
	SHA256  string
}

var debField = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*):[ ]?(.*)$`)

// Package name to served archive from the RFC822 style index, stanzas without file or hash dropped
func ParseTermuxIndex(text string) map[string]PoolPackage {
	pool := map[string]PoolPackage{}
	fields := map[string]string{}
	lines := append(pySplitLines(text), "")
	for _, line := range lines {
		if pyStrip(line) == "" {
			if row, ok := poolPackage(fields); ok {
				pool[row.Name] = row
			}
			fields = map[string]string{}
			continue
		}
		if r, _ := utf8.DecodeRuneInString(line); !pyIsSpace(r) {
			if m := debField.FindStringSubmatch(line); m != nil {
				fields[m[1]] = pyStrip(m[2])
			}
		}
	}
	return pool
}

// Splits like str splitlines across every line boundary Python recognizes
func pySplitLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch r {
		case '\r':
			lines = append(lines, text[start:i])
			if i+1 < len(text) && text[i+1] == '\n' {
				size = 2
			}
			start = i + size
		case '\n', '\x0b', '\x0c', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
			lines = append(lines, text[start:i])
			start = i + size
		}
		i += size
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// Installable archive of one stanza, ok false without name, filename or hash
func poolPackage(fields map[string]string) (PoolPackage, bool) {
	name, filename, sha := fields["Package"], fields["Filename"], fields["SHA256"]
	if name == "" || filename == "" || sha == "" {
		return PoolPackage{}, false
	}
	return PoolPackage{Name: name, Version: fields["Version"], URL: TermuxPool + filename, SHA256: sha}, true
}

// Fetches and parses the pool's current package index
func TermuxIndex() (map[string]PoolPackage, error) { return termuxIndex() }

// Index fetch behind TermuxIndex, the termux_libs index a test replaces
var termuxIndex = func() (map[string]PoolPackage, error) {
	text, err := GetText(TermuxIndexURL, nil)
	if err != nil {
		return nil, err
	}
	return ParseTermuxIndex(text), nil
}

// Archive hasher repair uses without a verifier, the termux_libs hash_url a test replaces
var termuxHashURL = HashURL

// Pool package a pinned URL names, empty for any other source, an error where urlsplit raises
func PackageOf(rawURL string) (string, error) {
	u, err := pyURLSplit(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Netloc != termuxPoolHost || !strings.HasPrefix(u.Path, termuxPoolPathPrefix) {
		return "", nil
	}
	parts := strings.Split(u.Path[len(termuxPoolPathPrefix):], "/")
	if len(parts) != 3 {
		return "", nil
	}
	for _, p := range parts {
		if p == "" {
			return "", nil
		}
	}
	return parts[1], nil
}

// URL parts as Python urlsplit returns them, nothing decoded
type pyURLParts struct {
	Scheme, Netloc, Path, Query, Fragment string
}

// Leading characters urlsplit strips, the WHATWG C0 controls and space
const whatwgC0ControlOrSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

var (
	urlSchemeChars = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	ipvFuture      = regexp.MustCompile(`^[vV][a-fA-F0-9]+\..+$`)
)

// Splits a URL the way Python urlsplit does, its ValueError cases returned as errors
func pyURLSplit(raw string) (pyURLParts, error) {
	var p pyURLParts
	rest := strings.TrimLeft(raw, whatwgC0ControlOrSpace)
	rest = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(rest)
	if i := strings.IndexByte(rest, ':'); i > 0 && urlSchemeChars.MatchString(rest[:i]) {
		p.Scheme, rest = strings.ToLower(rest[:i]), rest[i+1:]
	}
	if strings.HasPrefix(rest, "//") {
		end := len(rest)
		if j := strings.IndexAny(rest[2:], "/?#"); j >= 0 {
			end = j + 2
		}
		p.Netloc, rest = rest[2:end], rest[end:]
		open, close := strings.Contains(p.Netloc, "["), strings.Contains(p.Netloc, "]")
		if open != close {
			return p, errors.New("Invalid IPv6 URL")
		}
		if open {
			if err := checkBracketedNetloc(p.Netloc); err != nil {
				return p, err
			}
		}
	}
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest, p.Fragment = rest[:i], rest[i+1:]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, p.Query = rest[:i], rest[i+1:]
	}
	if err := checkNetlocNFKC(p.Netloc); err != nil {
		return p, err
	}
	p.Path = rest
	return p, nil
}

// Validates a bracketed authority host the way urlsplit does, rejecting text around the brackets
func checkBracketedNetloc(netloc string) error {
	hostPort := netloc[strings.LastIndex(netloc, "@")+1:]
	before, bracketed, open := strings.Cut(hostPort, "[")
	hostname := ""
	if open {
		if before != "" {
			return errors.New("Invalid IPv6 URL")
		}
		var port string
		hostname, port, _ = strings.Cut(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return errors.New("Invalid IPv6 URL")
		}
	} else {
		hostname, _, _ = strings.Cut(hostPort, ":")
	}
	if strings.HasPrefix(hostname, "v") || strings.HasPrefix(hostname, "V") {
		if !ipvFuture.MatchString(hostname) {
			return errors.New("IPvFuture address is invalid")
		}
		return nil
	}
	addr, err := netip.ParseAddr(hostname)
	if err != nil {
		return fmt.Errorf("%s does not appear to be an IPv4 or IPv6 address", pyRepr(hostname))
	}
	if addr.Is4() {
		return errors.New("An IPv4 address cannot be in brackets")
	}
	return nil
}

// Rejects a non ASCII authority whose NFKC form smuggles in URL delimiters
func checkNetlocNFKC(netloc string) error {
	if netloc == "" || isASCII(netloc) {
		return nil
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	normalized := norm.NFKC.String(n)
	if n != normalized && strings.ContainsAny(normalized, "/?#@:") {
		return fmt.Errorf("netloc '%s' contains invalid characters under NFKC normalization", netloc)
	}
	return nil
}

// Python str of an optional string, the empty string standing for None
func pyNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Final path segment of a URL
func urlFilename(rawURL string) string {
	return rawURL[strings.LastIndex(rawURL, "/")+1:]
}

// Pin is one URL the Termux pool serves, scoped as a library, the license package or a tool
type Pin struct {
	Scope   string
	Name    string
	URL     string
	Version string
}

// Retired is a pin whose archive the pool replaced, a nil replacement meaning the pool dropped it
type Retired struct {
	Pin         Pin
	Replacement *PoolPackage
}

// Python error for subscripting a decoded value with a string key it does not hold
func pySubscriptError(v any, key string) error {
	switch v.(type) {
	case *OrderedObject, map[string]any:
		return fmt.Errorf("KeyError: %s", pyRepr(key))
	case string:
		return errors.New("TypeError: string indices must be integers, not 'str'")
	case []any:
		return errors.New("TypeError: list indices must be integers or slices, not str")
	}
	return fmt.Errorf("TypeError: '%s' object is not subscriptable", pyTypeName(v))
}

// Field of an ordered row the way Python subscripting reads it, raising for a missing key or non dict
func orderedField(row any, key string) (any, error) {
	if obj, ok := row.(*OrderedObject); ok {
		if v, ok := obj.Get(key); ok {
			return v, nil
		}
	}
	return nil, pySubscriptError(row, key)
}

// Python str of a row field read with get and an empty default
func orderedFieldOr(row *OrderedObject, key string) string {
	v, ok := row.Get(key)
	if !ok {
		return ""
	}
	return pyStr(v)
}

// Every pinned URL the pool serves, the table's libraries and license plus the lockfile's bionic tools
func TermuxPins(table *OrderedObject, lockfile *Lockfile) ([]Pin, error) {
	var out []Pin
	if libsValue, _ := table.Get("libs"); truthyOrdered(libsValue) {
		libs, ok := libsValue.(*OrderedObject)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'items'", pyTypeName(libsValue))
		}
		for _, name := range libs.Keys {
			u, err := orderedField(libs.Values[name], "url")
			if err != nil {
				return nil, err
			}
			out = append(out, Pin{Scope: "library", Name: name, URL: pyStr(u), Version: orderedFieldOr(libs.Values[name].(*OrderedObject), "version")})
		}
	}
	if licenses, _ := table.Get("licenses"); truthyOrdered(licenses) {
		u, err := orderedField(licenses, "url")
		if err != nil {
			return nil, err
		}
		out = append(out, Pin{Scope: "license", Name: "termux-licenses", URL: pyStr(u), Version: orderedFieldOr(licenses.(*OrderedObject), "version")})
	}
	for _, name := range lockfile.Names() {
		artifacts := lockfile.PinnedArtifacts(name)
		for _, target := range sortedKeys(artifacts) {
			for _, row := range artifactRows(artifacts[target]) {
				m, ok := row.(map[string]any)
				if !ok {
					continue
				}
				u, _ := m["url"].(string)
				pkg, err := PackageOf(u)
				if err != nil {
					return nil, err
				}
				if pkg != "" {
					version, _ := lockfile.Version(name)
					out = append(out, Pin{Scope: "tool", Name: name + "@" + target, URL: u, Version: version})
				}
			}
		}
	}
	return out, nil
}

// Python truthiness of an ordered JSON value
func truthyOrdered(v any) bool {
	if obj, ok := v.(*OrderedObject); ok {
		return len(obj.Keys) > 0
	}
	return truthy(v)
}

// Rows of a target's pin, a single object standing for a one row list
func artifactRows(rows any) []any {
	if list, ok := rows.([]any); ok {
		return list
	}
	return []any{rows}
}

// Pins whose archive the pool replaced or dropped
func RetiredPins(table *OrderedObject, lockfile *Lockfile, pool map[string]PoolPackage) ([]Retired, error) {
	pins, err := TermuxPins(table, lockfile)
	if err != nil {
		return nil, err
	}
	var out []Retired
	for _, pin := range pins {
		pkg, err := PackageOf(pin.URL)
		if err != nil {
			return nil, err
		}
		entry, ok := pool[pkg]
		if !ok {
			out = append(out, Retired{Pin: pin})
			continue
		}
		if urlFilename(entry.URL) != urlFilename(pin.URL) {
			e := entry
			out = append(out, Retired{Pin: pin, Replacement: &e})
		}
	}
	return out, nil
}

// Repins every retired row to the pool's current archive after hashing its bytes, returning the count
func RepairTermuxPins(table *OrderedObject, lockfile *Lockfile, stale []Retired, verify func(string) (string, error)) (int, error) {
	if verify == nil {
		verify = termuxHashURL
	}
	applied := 0
	for _, entry := range stale {
		if entry.Replacement == nil {
			continue
		}
		served, err := verify(entry.Replacement.URL)
		if err != nil {
			return applied, err
		}
		if served != entry.Replacement.SHA256 {
			pkg, err := PackageOf(entry.Pin.URL)
			if err != nil {
				return applied, err
			}
			return applied, &PinMismatchError{Message: fmt.Sprintf("%s: the pool serves %s, the index declares %s",
				pyNone(pkg), served, entry.Replacement.SHA256)}
		}
		if err := repointPin(table, lockfile, entry); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

// Points one retired pin at its replacement in the table or the lockfile
func repointPin(table *OrderedObject, lockfile *Lockfile, entry Retired) error {
	pin, replacement := entry.Pin, entry.Replacement
	if pin.Scope == "tool" {
		name, target, _ := strings.Cut(pin.Name, "@")
		artifacts := lockfile.PinnedArtifacts(name)
		rows, ok := artifacts[target]
		if !ok {
			return fmt.Errorf("KeyError: %s", pyRepr(target))
		}
		pinPkg, err := PackageOf(pin.URL)
		if err != nil {
			return err
		}
		for _, row := range artifactRows(rows) {
			m, ok := row.(map[string]any)
			if !ok {
				return fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyTypeName(row))
			}
			u, _ := m["url"].(string)
			rowPkg, err := PackageOf(u)
			if err != nil {
				return err
			}
			if rowPkg == pinPkg {
				m["url"], m["sha256"] = replacement.URL, replacement.SHA256
			}
		}
		lockfile.setPinValue(name, lockfile.versionValue(name), artifacts)
		return nil
	}
	var row any
	var err error
	if pin.Scope == "library" {
		var libs any
		if libs, err = orderedField(table, "libs"); err == nil {
			row, err = orderedField(libs, pin.Name)
		}
	} else {
		row, err = orderedField(table, "licenses")
	}
	if err != nil {
		return err
	}
	obj, ok := row.(*OrderedObject)
	if !ok {
		if _, isList := row.([]any); isList {
			return errors.New("TypeError: list indices must be integers or slices, not str")
		}
		return fmt.Errorf("TypeError: '%s' object does not support item assignment", pyTypeName(row))
	}
	obj.Set("url", replacement.URL)
	obj.Set("sha256", replacement.SHA256)
	obj.Set("version", replacement.Version)
	return nil
}
