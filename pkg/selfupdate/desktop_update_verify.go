package selfupdate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Hidden entry verifying the packaged Desktop app at the Windows hand-off receipt boundary
const DesktopUpdateVerifyEntry = "__icebath_desktop_update_verify__"

// Packaged executable lookup, its integrity check and the checkout root the verify binds, the names hermes tests replace
var (
	verifyPackagedExecutable = desktopPackagedExecutable
	verifyExeIntegrityError  = desktopExeIntegrityError
	verifyCheckoutRoot       = checkoutRoot
)

// Failure the archive check catches and reports, its text the str of the Python exception
type asarInvalid struct{ text string }

func (e *asarInvalid) Error() string { return e.text }

// Value of node[key] with the KeyError or TypeError text Python subscripting raises
func asarItem(node any, key string) (any, error) {
	switch n := node.(type) {
	case *pyjson.Object:
		value, ok := n.Vals[key]
		if !ok {
			return nil, &asarInvalid{pyrepr.Repr(key)}
		}
		return value, nil
	case []any:
		return nil, &asarInvalid{"list indices must be integers or slices, not str"}
	case string:
		return nil, &asarInvalid{"string indices must be integers, not 'str'"}
	}
	return nil, &asarInvalid{fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(node))}
}

// Integer of a JSON value isinstance int accepts, bool included
func asarInt(value any) (*big.Int, bool) {
	if b, ok := value.(bool); ok {
		if b {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	}
	return pyrepr.Int(value)
}

// Size of the archive as stat reports it, the OSError text when the stat fails
func asarSize(archive string) (*big.Int, error) {
	info, err := os.Stat(archive)
	if err != nil {
		return nil, &asarInvalid{pypath.OSErrorText(err)}
	}
	return big.NewInt(info.Size()), nil
}

// Reads the ASAR Pickle header and the main entry packaged package.json declares, then the renderer module entry
func verifyPackagedEntry(resources string) error {
	if err := verifyPackagedArchive(resources); err != nil {
		var invalid *asarInvalid
		if !errors.As(err, &invalid) {
			return err
		}
		return runtimeErr("The updated Desktop archive or main entry is invalid: " + invalid.text)
	}
	index := filepath.Join(resources, "app.asar.unpacked", "dist", "index.html")
	raw, err := os.ReadFile(index)
	if err != nil {
		return runtimeErr("The updated Desktop renderer entry is invalid: " + pypath.OSErrorText(err))
	}
	html, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return runtimeErr("The updated Desktop renderer entry is invalid: " + err.Error())
	}
	for _, match := range htmlTagWithURL.FindAllStringSubmatch(html, -1) {
		if moduleTag.MatchString(match[0]) && strings.HasPrefix(strings.ToLower(match[0]), "<script") && !absoluteURL.MatchString(match[1]) {
			return nil
		}
	}
	return runtimeErr("The updated Desktop renderer entry is invalid: renderer has no local module entry")
}

// Archive header and main entry check, an asarInvalid for every failure the Python except clause catches
func verifyPackagedArchive(resources string) error {
	archive := filepath.Join(resources, "app.asar")
	stream, err := os.Open(archive)
	if err != nil {
		return &asarInvalid{pypath.OSErrorText(err)}
	}
	defer stream.Close()
	head := make([]byte, 16)
	n, err := io.ReadFull(stream, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &asarInvalid{pypath.OSErrorText(err)}
	}
	if n != 16 {
		return &asarInvalid{"unpack requires a buffer of 16 bytes"}
	}
	size := uint64(binary.LittleEndian.Uint32(head[0:]))
	headerSize := uint64(binary.LittleEndian.Uint32(head[4:]))
	payloadSize := uint64(binary.LittleEndian.Uint32(head[8:]))
	jsonSize := uint64(binary.LittleEndian.Uint32(head[12:]))
	if size != 4 || headerSize != payloadSize+4 || payloadSize != 4+((jsonSize+3)/4)*4 || !(jsonSize > 0 && jsonSize <= 64*1024*1024) {
		return &asarInvalid{"invalid ASAR header"}
	}
	archiveSize, err := asarSize(archive)
	if err != nil {
		return err
	}
	if new(big.Int).SetUint64(8+headerSize).Cmp(archiveSize) > 0 {
		return &asarInvalid{"invalid ASAR header"}
	}
	rawHeader := make([]byte, jsonSize)
	read, err := io.ReadFull(stream, rawHeader)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &asarInvalid{pypath.OSErrorText(err)}
	}
	header, err := pyjson.LoadsBytes(rawHeader[:read])
	if err != nil {
		return &asarInvalid{err.Error()}
	}
	readMember := func(nameValue any) ([]byte, error) {
		if !pyrepr.Truthy(nameValue) {
			return nil, &asarInvalid{"invalid ASAR entry path"}
		}
		name, ok := nameValue.(string)
		if !ok {
			return nil, &asarInvalid{"expected str, bytes or os.PathLike object, not " + pyrepr.TypeName(nameValue)}
		}
		var parts []string
		if pure := pypath.PosixPurePathStr(name); pure != "." {
			parts = strings.Split(pure, "/")
		}
		if pypath.PosixIsAbs(name) || slices.Contains(parts, "..") || strings.Contains(name, "\\") || strings.Contains(name, ":") {
			return nil, &asarInvalid{"invalid ASAR entry path"}
		}
		node := header
		for _, part := range parts {
			files, err := asarItem(node, "files")
			if err != nil {
				return nil, err
			}
			if node, err = asarItem(files, part); err != nil {
				return nil, err
			}
		}
		lengthValue, err := asarItem(node, "size")
		if err != nil {
			return nil, err
		}
		length, isInt := asarInt(lengthValue)
		if !isInt || length.Sign() <= 0 {
			return nil, &asarInvalid{"empty ASAR entry: " + name}
		}
		var data []byte
		if pyrepr.Truthy(node.(*pyjson.Object).GetOr("unpacked", nil)) {
			data, err = os.ReadFile(filepath.Join(resources, "app.asar.unpacked", filepath.FromSlash(strings.Join(parts, "/"))))
			if err != nil {
				return nil, &asarInvalid{pypath.OSErrorText(err)}
			}
		} else {
			offsetValue, err := asarItem(node, "offset")
			if err != nil {
				return nil, err
			}
			offset, err := pyint.Of(offsetValue)
			if err != nil {
				var intErr *pyint.Error
				if errors.As(err, &intErr) && intErr.Kind == "OverflowError" {
					return nil, &pm.PyError{Class: "OverflowError", Message: intErr.Msg}
				}
				return nil, &asarInvalid{err.Error()}
			}
			archiveSize, err := asarSize(archive)
			if err != nil {
				return nil, err
			}
			end := new(big.Int).SetUint64(8 + headerSize)
			end.Add(end, offset).Add(end, length)
			if offset.Sign() < 0 || end.Cmp(archiveSize) > 0 {
				return nil, &asarInvalid{"truncated ASAR entry: " + name}
			}
			start := int64(8+headerSize) + offset.Int64()
			if _, err := stream.Seek(start, io.SeekStart); err != nil {
				return nil, &asarInvalid{pypath.OSErrorText(err)}
			}
			data = make([]byte, length.Int64())
			got, err := io.ReadFull(stream, data)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, &asarInvalid{pypath.OSErrorText(err)}
			}
			data = data[:got]
		}
		if !length.IsInt64() || int64(len(data)) != length.Int64() || len(bytes.Trim(data, " \t\n\r\x0b\x0c")) == 0 {
			return nil, &asarInvalid{"incomplete ASAR entry: " + name}
		}
		return data, nil
	}
	rawPackage, err := readMember("package.json")
	if err != nil {
		return err
	}
	pkg, err := pyjson.LoadsBytes(rawPackage)
	if err != nil {
		return &asarInvalid{err.Error()}
	}
	mainValue, err := asarItem(pkg, "main")
	if err != nil {
		return err
	}
	mainEntry, err := readMember(mainValue)
	if err != nil {
		return err
	}
	if _, err := pystr.DecodeUTF8(mainEntry); err != nil {
		return &asarInvalid{err.Error()}
	}
	return nil
}

// Tree this binary runs from, the only root the receipt may describe
func checkoutRoot() string { return pm.RepoRoot() }

// Caller root while it carries a packaged app, else the checkout this binary runs from
func rootToVerify(projectRoot *string) string {
	if projectRoot == nil {
		return verifyCheckoutRoot()
	}
	if _, ok := verifyPackagedExecutable(filepath.Join(*projectRoot, "apps", "desktop")); ok {
		return *projectRoot
	}
	return verifyCheckoutRoot()
}

// Fails when a zero exit updater left an incomplete or stale packaged app
func verifyWindowsDesktopUpdate(ctx context.Context, projectRoot *string) error {
	root := rootToVerify(projectRoot)
	desktop := filepath.Join(root, "apps", "desktop")
	executable, ok := verifyPackagedExecutable(desktop)
	if !ok {
		return runtimeErr("The updated Desktop executable is missing")
	}
	if problem := verifyExeIntegrityError(executable); problem != "" {
		return runtimeErr("The updated Desktop executable is invalid: " + problem)
	}
	if err := verifyPackagedEntry(filepath.Join(filepath.Dir(executable), "resources")); err != nil {
		return err
	}
	if desktopBuildNeeded(ctx, desktop, root, false) {
		return runtimeErr("The updated Desktop build is stale, unstamped, or incomplete")
	}
	return nil
}

// Runs the verify for the hidden entry argv names and exits with its status
func DispatchDesktopUpdateVerifyEntry(ctx context.Context, exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != DesktopUpdateVerifyEntry {
		return
	}
	exit(desktopUpdateVerifyMain(ctx, os.Stderr))
}

// Exit status of the module run, an uncaught failure printed as its class and text
func desktopUpdateVerifyMain(ctx context.Context, stderr io.Writer) int {
	if err := verifyWindowsDesktopUpdate(ctx, nil); err != nil {
		if isInterrupt(err) {
			return 130
		}
		fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
		return 1
	}
	return 0
}
