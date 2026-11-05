package pm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var (
	rptIconv    = TermuxPool + "pool/main/libi/libiconv/libiconv_1.19_aarch64.deb"
	rptIconvOld = TermuxPool + "pool/main/libi/libiconv/libiconv_1.18-1_aarch64.deb"
	rptGlib     = TermuxPool + "pool/main/g/glib/glib_2.90.0_aarch64.deb"
	rptLicenses = TermuxPool + "pool/main/t/termux-licenses/termux-licenses_2.2_all.deb"
	rptUVOld    = TermuxPool + "pool/main/u/uv/uv_0.12.10_aarch64.deb"
	rptUVNew    = TermuxPool + "pool/main/u/uv/uv_0.12.15_aarch64.deb"
)

var rptIndex = "Package: libiconv\n" +
	"Version: 1.19\n" +
	"Architecture: aarch64\n" +
	"Filename: " + rptIconv[len(TermuxPool):] + "\n" +
	"Size: 562724\n" +
	"SHA256: " + strings.Repeat("i", 64) + "\n" +
	"Description: An implementation of iconv()\n" +
	" Depends: libc\n" +
	" This continuation line is not a stanza.\n" +
	"\n" +
	"Package: glib\n" +
	"Version: 2.90.0\n" +
	"Filename: " + rptGlib[len(TermuxPool):] + "\n" +
	"SHA256: " + strings.Repeat("g", 64) + "\n"

// Ordered object from alternating keys and values
func rptObj(pairs ...any) *OrderedObject {
	o := NewOrderedObject()
	for i := 0; i < len(pairs); i += 2 {
		o.Set(pairs[i].(string), pairs[i+1])
	}
	return o
}

// Plain decoded JSON tree of an ordered tree, for order blind equality
func rptPlain(v any) any {
	switch t := v.(type) {
	case *OrderedObject:
		out := map[string]any{}
		for _, k := range t.Keys {
			out[k] = rptPlain(t.Values[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = rptPlain(x)
		}
		return out
	}
	return v
}

// Python json dumps with sorted keys of an ordered tree
func rptSortedDump(t *testing.T, v any) string {
	t.Helper()
	body, err := dumpsJSON(rptPlain(v), false)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Lockfile holding packages written beside the test
func rptTermuxLockfile(t *testing.T, dir string, packages map[string]any) *Lockfile {
	t.Helper()
	path := filepath.Join(dir, "lock.json")
	body, err := dumpsJSON(map[string]any{"schema": jsonInt(LockSchema), "packages": packages}, false)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body, 0o666)
	return NewLockfile(path)
}

// Uv's two axes, astral's tarball on the desktop and the pool's deb on bionic
func rptUV(bionicURL string) map[string]any {
	return map[string]any{
		"version": "0.12.3",
		"artifacts": map[string]any{
			"darwin-x64": map[string]any{"url": "https://github.com/astral-sh/uv/releases/download/0.12.3/uv.tar.gz",
				"sha256": strings.Repeat("d", 64)},
			"linux-arm64-bionic": map[string]any{"url": bionicURL, "sha256": strings.Repeat("o", 64)},
		},
	}
}

// Pin table with the license package and two libraries
func rptTable(libiconvURL string) *OrderedObject {
	return rptObj(
		"licenses", rptObj("sha256", strings.Repeat("l", 64), "url", rptLicenses, "version", "2.2"),
		"libs", rptObj(
			"glib", rptObj("sha256", strings.Repeat("g", 64), "url", rptGlib, "version", "2.90.0"),
			"libiconv", rptObj("sha256", strings.Repeat("o", 64), "url", libiconvURL, "version", "1.18-1"),
		),
	)
}

// The pool the faked index reports, keyed by package
func rptPool(extra ...PoolPackage) map[string]PoolPackage {
	pool := map[string]PoolPackage{
		"libiconv":        {"libiconv", "1.19", rptIconv, strings.Repeat("i", 64)},
		"glib":            {"glib", "2.90.0", rptGlib, strings.Repeat("g", 64)},
		"termux-licenses": {"termux-licenses", "2.2", rptLicenses, strings.Repeat("l", 64)},
	}
	for _, row := range extra {
		pool[row.Name] = row
	}
	return pool
}

// Lookup verifier failing like a dict getitem for unserved URLs
func rptServed(served map[string]string) func(string) (string, error) {
	return func(u string) (string, error) {
		if sha, ok := served[u]; ok {
			return sha, nil
		}
		return "", fmt.Errorf("KeyError: %s", pyRepr(u))
	}
}

// Pins of a table and lockfile, failing the test on a raised error
func rptPins(t *testing.T, table *OrderedObject, lockfile *Lockfile) []Pin {
	t.Helper()
	pins, err := TermuxPins(table, lockfile)
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

// Retired pins against a pool, failing the test on a raised error
func rptRetired(t *testing.T, table *OrderedObject, lockfile *Lockfile, pool map[string]PoolPackage) []Retired {
	t.Helper()
	stale, err := RetiredPins(table, lockfile, pool)
	if err != nil {
		t.Fatal(err)
	}
	return stale
}

// Library row of the pin table
func rptLib(t *testing.T, table *OrderedObject, name string) any {
	t.Helper()
	libs, _ := table.Get("libs")
	row, ok := libs.(*OrderedObject).Get(name)
	if !ok {
		t.Fatalf("no library %s", name)
	}
	return rptPlain(row)
}

func TestTermuxParseIndexReadsStanzasAndIgnoresContinuations(t *testing.T) {
	isolateHome(t)
	pool := ParseTermuxIndex(rptIndex)
	if names := sortedKeys(pool); !reflect.DeepEqual(names, []string{"glib", "libiconv"}) {
		t.Fatalf("pool %v", names)
	}
	if want := (PoolPackage{"libiconv", "1.19", rptIconv, strings.Repeat("i", 64)}); pool["libiconv"] != want {
		t.Fatalf("libiconv %+v", pool["libiconv"])
	}
	if pool["glib"].SHA256 != strings.Repeat("g", 64) {
		t.Fatalf("glib %+v", pool["glib"])
	}
}

func TestTermuxParseIndexKeepsThePoolEpochAndDropsRowsWithoutAHash(t *testing.T) {
	isolateHome(t)
	text := "Package: libvpx\nVersion: 1:1.17.0\nFilename: pool/main/libv/libvpx/libvpx_1:1.17.0_aarch64.deb\n" +
		"SHA256: " + strings.Repeat("v", 64) + "\n\n" +
		"Package: half\nVersion: 1.0\nFilename: pool/main/h/half/half_1.0_aarch64.deb\n\n"
	pool := ParseTermuxIndex(text)
	if pool["libvpx"].Version != "1:1.17.0" || !strings.HasSuffix(pool["libvpx"].URL, "libvpx_1:1.17.0_aarch64.deb") {
		t.Fatalf("libvpx %+v", pool["libvpx"])
	}
	if _, ok := pool["half"]; ok {
		t.Fatal("a stanza without a hash was kept")
	}
}

func TestTermuxPackageOfRecognizesOnlyPoolArchives(t *testing.T) {
	isolateHome(t)
	for _, tc := range []struct{ url, want string }{
		{rptGlib, "glib"},
		{rptLicenses, "termux-licenses"},
		{"https://github.com/astral-sh/uv/releases/download/0.12.3/uv.tar.gz", ""},
		{"docker://termux/termux-docker@sha256:" + strings.Repeat("a", 64), ""},
		{TermuxPool + "dists/stable/main/binary-aarch64/Packages", ""},
		{TermuxPool + "pool/main/libi/libiconv/deeper/thing.deb", ""},
	} {
		if got, err := PackageOf(tc.url); err != nil || got != tc.want {
			t.Fatalf("PackageOf(%s) = %q %v, want %q", tc.url, got, err, tc.want)
		}
	}
}

func TestTermuxPinsSpanTheTableAndThePoolLockRows(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{"uv": rptUV(rptUVOld)})
	var names []string
	for _, pin := range rptPins(t, rptTable(rptIconvOld), lockfile) {
		names = append(names, pin.Scope+" "+pin.Name)
	}
	sort.Strings(names)
	want := []string{"library glib", "library libiconv", "license termux-licenses", "tool uv@linux-arm64-bionic"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("pins %v", names)
	}
}

func TestTermuxPinsIgnoreLockSourcesThePoolDoesNotServe(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{
		"uv": rptUV(rptUVOld),
		"chromium": map[string]any{"version": "145", "artifacts": map[string]any{"darwin-x64": map[string]any{
			"url":    "https://cdn.playwright.dev/builds/cft/145.0.7632.6/mac-x64/chrome-mac-x64.zip",
			"sha256": strings.Repeat("c", 64)}}},
	})
	var names []string
	for _, pin := range rptPins(t, rptObj("libs", NewOrderedObject()), lockfile) {
		names = append(names, pin.Name)
	}
	if !reflect.DeepEqual(names, []string{"uv@linux-arm64-bionic"}) {
		t.Fatalf("pins %v", names)
	}
}

func TestTermuxRetirementIsDecidedByTheFilenameNotTheVersionString(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{"uv": rptUV(rptUVOld)})
	table := rptTable(rptIconv)
	libs, _ := table.Get("libs")
	iconv, _ := libs.(*OrderedObject).Get("libiconv")
	iconv.(*OrderedObject).Set("version", "1.18-1")
	glib, _ := libs.(*OrderedObject).Get("glib")
	glib.(*OrderedObject).Set("version", "2.90.0-1")
	pool := rptPool(PoolPackage{"uv", "0.12.10-1", rptUVOld, strings.Repeat("o", 64)})
	if stale := rptRetired(t, table, lockfile, pool); len(stale) != 0 {
		t.Fatalf("retired %+v", stale)
	}
}

func TestTermuxRetiredReportsTheReplacementAndADroppedPackage(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{"uv": rptUV(TermuxPool + "pool/main/u/uv/uv_0.12.9_aarch64.deb")})
	table := rptTable(rptIconvOld)
	libs, _ := table.Get("libs")
	libs.(*OrderedObject).Set("nolonger", rptObj("sha256", strings.Repeat("n", 64), "version", "1",
		"url", TermuxPool+"pool/main/n/nolonger/nolonger_1_aarch64.deb"))
	pool := rptPool(PoolPackage{"uv", "0.12.15", rptUVNew, strings.Repeat("u", 64)})
	stale := map[string]Retired{}
	for _, entry := range rptRetired(t, table, lockfile, pool) {
		stale[entry.Pin.Name] = entry
	}
	if names := sortedKeys(stale); !reflect.DeepEqual(names, []string{"libiconv", "nolonger", "uv@linux-arm64-bionic"}) {
		t.Fatalf("retired %v", names)
	}
	if r := stale["libiconv"].Replacement; r == nil || r.URL != rptIconv {
		t.Fatalf("libiconv replacement %+v", r)
	}
	if r := stale["uv@linux-arm64-bionic"].Replacement; r == nil || r.URL != rptUVNew {
		t.Fatalf("uv replacement %+v", r)
	}
	if stale["nolonger"].Replacement != nil {
		t.Fatalf("dropped package got %+v", stale["nolonger"].Replacement)
	}
}

func TestTermuxRepairRepointsOnlyRetiredRows(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{"uv": rptUV(rptUVOld)})
	table := rptTable(rptIconvOld)
	served := rptServed(map[string]string{rptIconv: strings.Repeat("i", 64), rptUVNew: strings.Repeat("u", 64)})
	stale := rptRetired(t, table, lockfile, rptPool(PoolPackage{"uv", "0.12.15", rptUVNew, strings.Repeat("u", 64)}))
	applied, err := RepairTermuxPins(table, lockfile, stale, served)
	if err != nil || applied != 2 {
		t.Fatalf("applied %d err %v", applied, err)
	}
	if got := rptLib(t, table, "libiconv"); !reflect.DeepEqual(got, map[string]any{"sha256": strings.Repeat("i", 64), "url": rptIconv, "version": "1.19"}) {
		t.Fatalf("libiconv %v", got)
	}
	if got := rptLib(t, table, "glib"); !reflect.DeepEqual(got, map[string]any{"sha256": strings.Repeat("g", 64), "url": rptGlib, "version": "2.90.0"}) {
		t.Fatalf("glib %v", got)
	}
	licenses, _ := table.Get("licenses")
	if got := rptPlain(licenses); !reflect.DeepEqual(got, map[string]any{"sha256": strings.Repeat("l", 64), "url": rptLicenses, "version": "2.2"}) {
		t.Fatalf("licenses %v", got)
	}
	row := lockfile.Artifacts("uv", "linux-arm64-bionic")[0]
	if row["url"] != rptUVNew || row["sha256"] != strings.Repeat("u", 64) {
		t.Fatalf("bionic row %v", row)
	}
	if u, _ := lockfile.Artifacts("uv", "darwin-x64")[0]["url"].(string); !strings.HasPrefix(u, "https://github.com/") {
		t.Fatalf("desktop row moved to %s", u)
	}
}

func TestTermuxRepairRefusesBytesTheIndexDoesNotDescribe(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{"uv": rptUV(rptUVOld)})
	table := rptTable(rptIconvOld)
	before := rptSortedDump(t, table)
	stale := rptRetired(t, table, lockfile, rptPool(PoolPackage{"uv", "0.12.15", rptUVNew, strings.Repeat("u", 64)}))
	_, err := RepairTermuxPins(table, lockfile, stale, func(string) (string, error) { return strings.Repeat("z", 64), nil })
	var mismatch *PinMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error %v", err)
	}
	if rptSortedDump(t, table) != before {
		t.Fatal("the table changed after a refused repair")
	}
	if u := lockfile.Artifacts("uv", "linux-arm64-bionic")[0]["url"]; u != rptUVOld {
		t.Fatalf("bionic row moved to %v", u)
	}
}

func TestTermuxSaveTableRoundTripsWithLFAndKeepsRowOrder(t *testing.T) {
	tmp := isolateHome(t)
	path := filepath.Join(tmp, "table.json")
	table := rptTable(rptIconvOld)
	if err := SaveTermuxTable(table, path); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(readFile(t, path), []byte("\r\n")) {
		t.Fatal("the table was written with CRLF")
	}
	loaded, err := LoadTermuxTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rptPlain(loaded), rptPlain(table)) {
		t.Fatalf("loaded %v", rptPlain(loaded))
	}
	if !reflect.DeepEqual(loaded.Keys, []string{"licenses", "libs"}) {
		t.Fatalf("keys %v", loaded.Keys)
	}
}

// Saved table bytes equal Python json dumps with indent two, key order kept and nothing HTML escaped
func TestTermuxSaveTableWritesPythonJSONBytes(t *testing.T) {
	tmp := isolateHome(t)
	path := filepath.Join(tmp, "table.json")
	table := rptObj(
		"licenses", rptObj("url", "x", "version", "2.2"),
		"libs", rptObj(
			"a<b&c>", rptObj("n", []any{json.Number("1"), json.Number("2.5"), nil, true, "café \U0001F600"}),
			"empty", NewOrderedObject(),
			"list", []any{},
		),
	)
	if err := SaveTermuxTable(table, path); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"licenses\": {\n    \"url\": \"x\",\n    \"version\": \"2.2\"\n  },\n  \"libs\": {\n    \"a<b&c>\": {\n      \"n\": [\n        1,\n        2.5,\n        null,\n        true,\n        \"caf\\u00e9 \\ud83d\\ude00\"\n      ]\n    },\n    \"empty\": {},\n    \"list\": []\n  }\n}\n"
	if got := string(readFile(t, path)); got != want {
		t.Fatalf("saved\n%s\nwant\n%s", got, want)
	}
}

// Pool URLs split the way urlsplit splits them, raw path, whole netloc and its ValueError cases
func TestTermuxPackageOfSplitsLikePythonURLSplit(t *testing.T) {
	isolateHome(t)
	for raw, want := range map[string]string{
		"https://user@packages.termux.dev/apt/termux-main/pool/main/g/glib/x.deb":  "",
		"https://packages.termux.dev:443/apt/termux-main/pool/main/g/glib/x.deb":   "",
		"HTTPS://packages.termux.dev/apt/termux-main/pool/main/g/glib/x.deb":       "glib",
		"  https://packages.termux.dev/apt/termux-main/pool/main/g/gl\tib/x.deb":   "glib",
		"https://packages.termux.dev/apt/termux-main/pool/main/g/a%2Fb/x.deb":      "a%2Fb",
		"https://packages.termux.dev/apt/termux-main/pool/main/g/glib/x.deb?q=1#f": "glib",
		"https://[v1.x]/apt": "",
	} {
		if got, err := PackageOf(raw); err != nil || got != want {
			t.Fatalf("PackageOf(%q) = %q %v, want %q", raw, got, err, want)
		}
	}
	for raw, want := range map[string]string{
		"https://[::1/apt":          "Invalid IPv6 URL",
		"https://[127.0.0.1]/apt":   "An IPv4 address cannot be in brackets",
		"https://[::1]x/apt":        "Invalid IPv6 URL",
		"https://[vZ.x]/apt":        "IPvFuture address is invalid",
		"https://exa\u2100mple/apt": "netloc 'exa\u2100mple' contains invalid characters under NFKC normalization",
	} {
		if _, err := PackageOf(raw); err == nil || err.Error() != want {
			t.Fatalf("PackageOf(%q) error %v, want %s", raw, err, want)
		}
	}
}

// Malformed pin rows raise the errors Python pins and repoint raise
func TestTermuxPinsRaiseOnMalformedRows(t *testing.T) {
	tmp := isolateHome(t)
	lockfile := rptTermuxLockfile(t, tmp, map[string]any{})
	for _, tc := range []struct {
		table *OrderedObject
		want  string
	}{
		{rptObj("libs", rptObj("glib", rptObj("version", "1"))), "KeyError: 'url'"},
		{rptObj("libs", rptObj("glib", "text")), "TypeError: string indices must be integers, not 'str'"},
		{rptObj("libs", rptObj("glib", []any{"x"})), "TypeError: list indices must be integers or slices, not str"},
		{rptObj("libs", rptObj("glib", nil)), "TypeError: 'NoneType' object is not subscriptable"},
		{rptObj("libs", []any{"x"}), "AttributeError: 'list' object has no attribute 'items'"},
		{rptObj("licenses", rptObj("version", "2.2")), "KeyError: 'url'"},
	} {
		if _, err := TermuxPins(tc.table, lockfile); err == nil || err.Error() != tc.want {
			t.Fatalf("TermuxPins error %v, want %s", err, tc.want)
		}
	}
	if pins := rptPins(t, rptObj("libs", []any{}, "licenses", NewOrderedObject()), lockfile); len(pins) != 0 {
		t.Fatalf("falsy sections gave %v", pins)
	}
	mixed := rptTermuxLockfile(t, tmp, map[string]any{"uv": map[string]any{"version": "1", "artifacts": map[string]any{
		"linux-arm64-bionic": []any{map[string]any{"url": rptUVOld, "sha256": strings.Repeat("o", 64)}, "stray"}}}})
	stale := rptRetired(t, NewOrderedObject(), mixed, rptPool(PoolPackage{"uv", "0.12.15", rptUVNew, strings.Repeat("u", 64)}))
	_, err := RepairTermuxPins(NewOrderedObject(), mixed, stale, rptServed(map[string]string{rptUVNew: strings.Repeat("u", 64)}))
	if err == nil || err.Error() != "AttributeError: 'str' object has no attribute 'get'" {
		t.Fatalf("repoint error %v", err)
	}
}
