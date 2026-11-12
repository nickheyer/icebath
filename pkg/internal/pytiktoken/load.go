package pytiktoken

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
)

// Reads a local path or downloads an http url, raising for an error status like requests
func readFile(blobpath string) ([]byte, error) {
	if !strings.Contains(blobpath, "://") {
		return os.ReadFile(blobpath)
	}
	req, err := http.NewRequest(http.MethodGet, blobpath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := pyrequests.Send(context.Background(), http.DefaultTransport.(*http.Transport), pyrequests.Timeout{}, req)
	if err != nil {
		return nil, err
	}
	if err := resp.RaiseForStatus(blobpath); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func checkHash(data []byte, expected string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expected
}

// Lowercase hex of sixteen random bytes in the uuid4 layout
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Blob contents through the TIKTOKEN_CACHE_DIR, DATA_GYM_CACHE_DIR or temp data-gym-cache copy, re-fetched on a hash mismatch
func ReadFileCached(blobpath, expectedHash string) ([]byte, error) {
	userSpecified := true
	cacheDir, ok := os.LookupEnv("TIKTOKEN_CACHE_DIR")
	if !ok {
		cacheDir, ok = os.LookupEnv("DATA_GYM_CACHE_DIR")
	}
	if !ok {
		tmp, err := pypath.Gettempdir()
		if err != nil {
			return nil, err
		}
		cacheDir = filepath.Join(tmp, "data-gym-cache")
		userSpecified = false
	}
	if cacheDir == "" {
		return readFile(blobpath)
	}
	key := sha1.Sum([]byte(blobpath))
	cachePath := filepath.Join(cacheDir, hex.EncodeToString(key[:]))
	if _, err := os.Stat(cachePath); err == nil {
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return nil, err
		}
		if expectedHash == "" || checkHash(data, expectedHash) {
			return data, nil
		}
		_ = os.Remove(cachePath)
	}
	contents, err := readFile(blobpath)
	if err != nil {
		return nil, err
	}
	if expectedHash != "" && !checkHash(contents, expectedHash) {
		return nil, fmt.Errorf("ValueError: Hash mismatch for data downloaded from %s (expected %s). This may indicate a corrupted download. Please try again.", blobpath, expectedHash)
	}
	if werr := writeCache(cacheDir, cachePath, contents); werr != nil && userSpecified {
		return nil, werr
	}
	return contents, nil
}

// Writes the cache file through a uuid named temp file renamed into place
func writeCache(cacheDir, cachePath string, contents []byte) error {
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return err
	}
	tmp := cachePath + "." + uuid4() + ".tmp"
	if err := os.WriteFile(tmp, contents, 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath)
}

// Splits bytes on the newline, carriage return and CRLF breaks bytes.splitlines knows
func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexAny(b, "\r\n")
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i])
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			i++
		}
		b = b[i+1:]
	}
	return out
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// Mergeable ranks of a tiktoken BPE file, one base64 token and its rank per line
func LoadTiktokenBPE(blobpath, expectedHash string) (map[string]int, error) {
	contents, err := ReadFileCached(blobpath, expectedHash)
	if err != nil {
		return nil, err
	}
	ret := map[string]int{}
	for _, line := range splitLines(contents) {
		if len(line) == 0 {
			continue
		}
		fields := bytes.FieldsFunc(line, func(r rune) bool { return r < 0x80 && isASCIISpace(byte(r)) })
		token, rank, perr := parseRankLine(fields)
		if perr != nil {
			return nil, fmt.Errorf("ValueError: Error parsing line %s in %s", pyrepr.BytesRepr(line), blobpath)
		}
		ret[string(token)] = rank
	}
	return ret, nil
}

// One base64 token and its integer rank, an error for any other field count or value
func parseRankLine(fields [][]byte) ([]byte, int, error) {
	if len(fields) != 2 {
		return nil, 0, errors.New("expected two fields")
	}
	token, err := pybase64.B64Decode(string(fields[0]))
	if err != nil {
		return nil, 0, err
	}
	for _, c := range fields[1] {
		if c >= 0x80 {
			return nil, 0, errors.New("non ascii rank")
		}
	}
	n, ok := pyint.Parse(string(fields[1]))
	if !ok || !n.IsInt64() {
		return nil, 0, errors.New("invalid rank")
	}
	return token, int(n.Int64()), nil
}
