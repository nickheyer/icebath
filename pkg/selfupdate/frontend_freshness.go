package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	frontendReceiptName = "hermes-build.json"
	frontendStaleMarker = ".icebath-stale"
)

var (
	frontendWorkspaces = map[string]string{"tui": "ui-tui", "web": "web", "desktop": "apps/desktop"}
	frontendGenerated  = map[string]bool{"node_modules": true, "dist": true, "dist-webapp": true, "build": true, "release": true,
		".cache": true, ".git": true, "coverage": true, "test-results": true, "playwright-report": true}
	frontendTestFile  = regexp.MustCompile(`\.(test|spec)(-d)?\.[cm]?[jt]sx?$`)
	frontendTUIInputs = func() []string {
		var out []string
		for _, root := range []string{"ui-tui", "ui-tui/packages/hermes-ink", "apps/shared"} {
			out = append(out, root+"/src", root+"/package.json", root+"/tsconfig.json")
		}
		return append(out, "tsconfig.json", "package.json", "package-lock.json", ".npmrc", "pm/lock.json",
			"scripts/build/tui.mjs", "scripts/build/frontend-common.mjs", "scripts/build/freshness.mjs",
			"ui-tui/scripts/gateway_bootstrap.py", "ui-tui/scripts/clipboard_image.py")
	}()
	frontendWebappInputs = []string{"apps/desktop", "apps/shared", "package.json", "package-lock.json", ".npmrc", "pm/lock.json",
		"scripts/build/webapp.mjs", "scripts/build/node-deps.mjs", "scripts/build/frontend-common.mjs", "scripts/build/freshness.mjs"}
)

// Source paths whose bytes decide whether a product is current
func frontendProductInputs(product string) ([]string, error) {
	switch product {
	case "tui":
		return frontendTUIInputs, nil
	case "webapp":
		return frontendWebappInputs, nil
	}
	workspace, ok := frontendWorkspaces[product]
	if !ok {
		return nil, fmt.Errorf("Unknown frontend product: %s", product)
	}
	return []string{workspace, "apps/shared", "package.json", "package-lock.json", ".npmrc", "pm/lock.json", "scripts/build",
		"scripts/brand", "assets", "pyproject.toml", "uv.lock"}, nil
}

func frontendOSMetadata(name string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	return base == ".DS_Store" || base == ".localized" || base == "Thumbs.db" || base == "Desktop.ini" || strings.HasPrefix(base, "._")
}

// Generated trees, caches and per product test files a source hash leaves out
func frontendSkipInput(product string) func(string) bool {
	return func(name string) bool {
		parts := strings.Split(name, "/")
		for _, p := range parts {
			if frontendGenerated[p] && !strings.HasPrefix(name, "scripts/") {
				return true
			}
			if strings.HasPrefix(p, ".dist-") || strings.HasPrefix(p, ".staging-") || p == "__pycache__" {
				return true
			}
			if product == "tui" && p == "__tests__" {
				return true
			}
		}
		if product == "tui" && frontendTestFile.MatchString(name) {
			return true
		}
		return strings.HasSuffix(name, ".tsbuildinfo") || strings.HasSuffix(name, ".pyc")
	}
}

// Visits every input in order, directories recursing in sorted child order, missing inputs reported as such
func frontendWalkTree(root string, inputs []string, skip func(string) bool, onEntry func(name, kind string) error) error {
	var visit func(name string) error
	visit = func(name string) error {
		if frontendOSMetadata(name) || skip(name) {
			return nil
		}
		file := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Stat(file)
		if err != nil {
			return onEntry(name, "missing")
		}
		if !info.IsDir() {
			return onEntry(name, "file")
		}
		if err := onEntry(name, "directory"); err != nil {
			return err
		}
		entries, err := os.ReadDir(file)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, jsDecodeUTF8(e.Name()))
		}
		sort.Slice(names, func(i, j int) bool { return jsLess(names[i], names[j]) })
		for _, child := range names {
			if err := visit(name + "/" + child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, input := range inputs {
		if err := visit(input); err != nil {
			return err
		}
	}
	return nil
}

// Digest of names, kinds and file bytes of a tree, contents false hashing a file by its kind only
func frontendTreeHash(root string, inputs []string, skip func(string) bool, contents func(string) bool) (string, error) {
	h := sha256.New()
	err := frontendWalkTree(root, inputs, skip, func(name, kind string) error {
		h.Write([]byte(strings.ReplaceAll(name, `\`, "/")))
		h.Write([]byte{0})
		if kind != "file" {
			h.Write([]byte(kind + "\x00"))
			return nil
		}
		if contents != nil && !contents(name) {
			h.Write([]byte("file\x00"))
			return nil
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		h.Write(raw)
		h.Write([]byte{0})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func frontendSourceHash(source, product string) (string, error) {
	inputs, err := frontendProductInputs(product)
	if err != nil {
		return "", err
	}
	return frontendTreeHash(source, inputs, frontendSkipInput(product), nil)
}

func frontendNoSkip(string) bool { return false }

// Digest of a built product without its receipt, marker and ignored names, node_modules and native files by kind
func frontendOutputHash(out string, ignored ...string) (string, error) {
	entries, err := os.ReadDir(out)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		names = append(names, jsDecodeUTF8(e.Name()))
	}
	sort.Slice(names, func(i, j int) bool { return jsLess(names[i], names[j]) })
	skip := func(name string) bool {
		if name == frontendReceiptName || name == ".icebath-product" {
			return true
		}
		for _, i := range ignored {
			if name == i {
				return true
			}
		}
		return false
	}
	contents := func(name string) bool {
		for _, p := range strings.Split(name, "/") {
			if p == "node_modules" {
				return false
			}
		}
		return !strings.HasPrefix(name, "native/")
	}
	return frontendTreeHash(out, names, skip, contents)
}

// JSON.stringify of a decoded JSON value, objects in key order and numbers in JavaScript form
func jsStringify(v any) string {
	var b strings.Builder
	var write func(v any)
	write = func(v any) {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			b.WriteString(strconv.FormatBool(x))
		case string:
			b.WriteString(jsQuote(x))
		case int:
			b.WriteString(strconv.Itoa(x))
		case int64:
			b.WriteString(jsNumber(float64(x)))
		case *big.Int:
			f, _ := new(big.Float).SetInt(x).Float64()
			b.WriteString(jsNumber(f))
		case float64:
			b.WriteString(jsNumber(x))
		case []any:
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				write(e)
			}
			b.WriteByte(']')
		case []string:
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(jsQuote(e))
			}
			b.WriteByte(']')
		case *pyjson.Object:
			b.WriteByte('{')
			for i, k := range jsKeyOrder(x.Keys) {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(jsQuote(k) + ":")
				write(x.Vals[k])
			}
			b.WriteByte('}')
		default:
			b.WriteString(jsQuote(fmt.Sprint(x)))
		}
	}
	write(v)
	return b.String()
}

// Property order of a JavaScript object, array index keys ascending first and the rest in insertion order
func jsKeyOrder(keys []string) []string {
	var index, rest []string
	for _, k := range keys {
		if n, err := strconv.ParseUint(k, 10, 32); err == nil && n < math.MaxUint32 && strconv.FormatUint(n, 10) == k {
			index = append(index, k)
		} else {
			rest = append(rest, k)
		}
	}
	sort.SliceStable(index, func(i, j int) bool {
		a, _ := strconv.ParseUint(index[i], 10, 32)
		c, _ := strconv.ParseUint(index[j], 10, 32)
		return a < c
	})
	return append(index, rest...)
}

// Leading rune of s as the WHATWG UTF-8 decoder Node uses reads it, one replacement per maximal invalid subpart
func jsDecodeRune(s string) (rune, int) {
	r, size := utf8.DecodeRuneInString(s)
	if r != utf8.RuneError || size != 1 {
		return r, size
	}
	lead := s[0]
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead >= 0xE1 && lead <= 0xEF:
		need = 2
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead == 0xF4:
		need, hi = 3, 0x8F
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	default:
		return utf8.RuneError, 1
	}
	i := 1
	for ; i <= need && i < len(s); i++ {
		if s[i] < lo || s[i] > hi {
			break
		}
		lo, hi = 0x80, 0xBF
	}
	return utf8.RuneError, i
}

// Text of s as Node decodes bytes to a string, each invalid subpart a replacement character
func jsDecodeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := jsDecodeRune(s[i:])
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

// JSON string literal as JavaScript writes it, lone surrogates escaped
func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r, size = jsDecodeRune(s[i:])
		}
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || utf16.IsSurrogate(r) {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}

// Number toString of ECMAScript, integers below 1e21 in plain digits and exponents past it or below 1e-6
func jsNumber(f float64) string {
	switch {
	case math.IsNaN(f) || math.IsInf(f, 0):
		return "null"
	case f == 0:
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expS, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expS)
	k, n := len(digits), exp+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	expSign := "+"
	if n-1 < 0 {
		expSign = "-"
	}
	frac := ""
	if k > 1 {
		frac = "." + digits[1:]
	}
	return sign + digits[:1] + frac + "e" + expSign + strconv.Itoa(int(math.Abs(float64(n-1))))
}

// Parsed stamp object, nil when absent, unparsable or not an object
func frontendReadStamp(path string) *pyjson.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	obj, _ := v.(*pyjson.Object)
	return obj
}

// Digest of a stamp's identity without its clock fields, the tree hash when it is not a JSON object
func frontendStampContentHash(path string) (string, error) {
	stamp := frontendReadStamp(path)
	if stamp == nil {
		return frontendTreeHash(path, []string{"."}, frontendNoSkip, nil)
	}
	keys := make([]string, 0, len(stamp.Keys))
	for _, k := range stamp.Keys {
		if k != "builtAt" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return jsLess(keys[i], keys[j]) })
	identity := pyjson.NewObject()
	for _, k := range keys {
		identity.Set(k, stamp.Vals[k])
	}
	sum := sha256.Sum256([]byte(jsStringify(identity)))
	return hex.EncodeToString(sum[:]), nil
}

// String order of JavaScript, by UTF-16 code units
func jsLess(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// Clock the output bakes from the prepared stamp, nil when there is none
func frontendStampClock(prepared map[string]string) any {
	path, ok := prepared["stamp"]
	if !ok || path == "" {
		return nil
	}
	abs, _ := filepath.Abs(path)
	stamp := frontendReadStamp(abs)
	if stamp == nil {
		return nil
	}
	v, ok := stamp.Get("builtAt")
	if !ok {
		return nil
	}
	return v
}

// Build inputs record of a product, its source hash, prepared trees and for renderers the VITE environment
func frontendBuildInputs(source, product string, prepared map[string]string, env map[string]string) (*pyjson.Object, error) {
	sourceHash, err := frontendSourceHash(source, product)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(prepared))
	for n := range prepared {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return jsLess(names[i], names[j]) })
	list := []any{}
	for _, name := range names {
		abs, _ := filepath.Abs(prepared[name])
		var hash string
		if name == "stamp" {
			hash, err = frontendStampContentHash(abs)
		} else {
			hash, err = frontendTreeHash(abs, []string{"."}, frontendNoSkip, nil)
		}
		if err != nil {
			return nil, err
		}
		list = append(list, pyjson.ObjectOf("name", name, "path", abs, "hash", hash))
	}
	inputs := pyjson.ObjectOf("sourceHash", sourceHash, "prepared", list)
	if product == "desktop" || product == "webapp" {
		renderer := map[string]string{}
		for k, v := range env {
			renderer[k] = v
		}
		if renderer["NODE_ENV"] == "" {
			renderer["NODE_ENV"] = "production"
		}
		keys := []string{}
		for k := range renderer {
			if k == "NODE_ENV" || strings.HasPrefix(k, "VITE_") {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool { return jsLess(keys[i]+","+renderer[keys[i]], keys[j]+","+renderer[keys[j]]) })
		pairs := []any{}
		for _, k := range keys {
			pairs = append(pairs, []any{k, renderer[k]})
		}
		inputs.Set("env", pairs)
	}
	return inputs, nil
}

// Prepared name to path map a receipt's inputs recorded
func frontendPreparedPaths(inputs *pyjson.Object) map[string]string {
	out := map[string]string{}
	list, _ := inputs.Get("prepared")
	items, _ := list.([]any)
	for _, item := range items {
		obj, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		name, _ := obj.Get("name")
		path, _ := obj.Get("path")
		ns, _ := name.(string)
		ps, _ := path.(string)
		out[ns] = ps
	}
	return out
}

// Platform, arch and version names Node reports for the node binary that runs a build
type nodeHost struct{ platform, arch, version string }

func frontendNodeHost(node string) (nodeHost, error) {
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		return nodeHost{}, err
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	arch := map[string]string{"amd64": "x64", "386": "ia32", "ppc64le": "ppc64", "mipsle": "mipsel"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	return nodeHost{platform, arch, strings.TrimPrefix(strings.TrimSpace(string(out)), "v")}, nil
}

// Writes the receipt of a compiled product after confirming its inputs did not move during the build
func frontendRecordProduct(host nodeHost, source, product, out string, inputs *pyjson.Object, env map[string]string, stampClock any, hasClock bool) error {
	again, err := frontendBuildInputs(source, product, frontendPreparedPaths(inputs), env)
	if err != nil {
		return err
	}
	if jsStringify(again) != jsStringify(inputs) {
		return errors.New("Build inputs changed during compilation; retry the build")
	}
	outputHash, err := frontendOutputHash(out)
	if err != nil {
		return err
	}
	receipt := pyjson.ObjectOf("schema", int64(1), "product", product, "platform", host.platform, "arch", host.arch,
		"node", host.version, "inputs", inputs)
	if hasClock {
		receipt.Set("stampClock", stampClock)
	}
	receipt.Set("outputHash", outputHash)
	return os.WriteFile(filepath.Join(out, frontendReceiptName), []byte(jsStringify(receipt)+"\n"), 0o666)
}

// Receipt of out when written for product by this platform, arch and node, nil otherwise
func frontendHostReceipt(host nodeHost, out, product string) *pyjson.Object {
	raw, err := os.ReadFile(filepath.Join(out, frontendReceiptName))
	if err != nil {
		return nil
	}
	v, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil
	}
	saved, ok := v.(*pyjson.Object)
	if !ok {
		return nil
	}
	get := func(k string) any { x, _ := saved.Get(k); return x }
	if jsStringify(get("schema")) != "1" || get("product") != product || get("platform") != host.platform ||
		get("arch") != host.arch || get("node") != host.version {
		return nil
	}
	return saved
}

// True when the receipt in out matches the current inputs, clock and output bytes for this host
func frontendProductCurrent(host nodeHost, source, product, out string, prepared map[string]string, env map[string]string) bool {
	saved := frontendHostReceipt(host, out, product)
	if saved == nil {
		return false
	}
	inputsV, _ := saved.Get("inputs")
	inputs, ok := inputsV.(*pyjson.Object)
	if !ok {
		return false
	}
	paths := prepared
	if paths == nil {
		paths = frontendPreparedPaths(inputs)
	}
	if clock, has := saved.Get("stampClock"); has && jsStringify(clock) != jsStringify(frontendStampClock(paths)) {
		return false
	}
	current, err := frontendBuildInputs(source, product, paths, env)
	if err != nil || jsStringify(inputs) != jsStringify(current) {
		return false
	}
	outputHash, err := frontendOutputHash(out)
	savedHash, _ := saved.Get("outputHash")
	return err == nil && savedHash == outputHash
}
