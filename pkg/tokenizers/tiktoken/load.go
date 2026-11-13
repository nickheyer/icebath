package tiktoken

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Reads a local path or fetches an http url, an error for any status but 200
func readFile(blobpath string) ([]byte, error) {
	if !strings.Contains(blobpath, "://") {
		return os.ReadFile(blobpath)
	}
	resp, err := http.Get(blobpath)
	if err != nil {
		return nil, fmt.Errorf("tiktoken: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tiktoken: %s answered %s", blobpath, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func checkHash(data []byte, expected string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expected
}

// ReadFileCached returns a rank file through the TIKTOKEN_CACHE_DIR, DATA_GYM_CACHE_DIR or temp data-gym-cache copy, fetched again on a hash mismatch
func ReadFileCached(blobpath, expectedHash string) ([]byte, error) {
	userSpecified := true
	cacheDir, ok := os.LookupEnv("TIKTOKEN_CACHE_DIR")
	if !ok {
		cacheDir, ok = os.LookupEnv("DATA_GYM_CACHE_DIR")
	}
	if !ok {
		cacheDir = filepath.Join(os.TempDir(), "data-gym-cache")
		userSpecified = false
	}
	if cacheDir == "" {
		return readFile(blobpath)
	}
	key := sha1.Sum([]byte(blobpath))
	cachePath := filepath.Join(cacheDir, hex.EncodeToString(key[:]))
	if data, err := os.ReadFile(cachePath); err == nil {
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
		return nil, fmt.Errorf("tiktoken: %s does not have the sha256 %s", blobpath, expectedHash)
	}
	if werr := writeCache(cacheDir, cachePath, contents); werr != nil && userSpecified {
		return nil, werr
	}
	return contents, nil
}

// Writes the cache file through a randomly named temp file renamed into place
func writeCache(cacheDir, cachePath string, contents []byte) error {
	if err := os.MkdirAll(cacheDir, 0o777); err != nil {
		return err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := cachePath + "." + hex.EncodeToString(nonce[:]) + ".tmp"
	if err := os.WriteFile(tmp, contents, 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath)
}

// ParseRanks reads a rank file, one base64 token and its rank per line
func ParseRanks(contents []byte) (map[string]int, error) {
	ranks := map[string]int{}
	for n, line := range bytes.Split(contents, []byte("\n")) {
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("tiktoken: line %d holds %d fields, a token and its rank are two", n+1, len(fields))
		}
		token, err := base64.StdEncoding.DecodeString(string(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("tiktoken: line %d: %w", n+1, err)
		}
		rank, err := strconv.Atoi(string(fields[1]))
		if err != nil {
			return nil, fmt.Errorf("tiktoken: line %d: %w", n+1, err)
		}
		ranks[string(token)] = rank
	}
	return ranks, nil
}

// LoadRanks reads the rank file at a path or url through the cache, checked against a sha256 when one is given
func LoadRanks(blobpath, expectedHash string) (map[string]int, error) {
	contents, err := ReadFileCached(blobpath, expectedHash)
	if err != nil {
		return nil, err
	}
	ranks, err := ParseRanks(contents)
	if err != nil {
		return nil, fmt.Errorf("%w in %s", err, blobpath)
	}
	return ranks, nil
}
