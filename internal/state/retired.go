package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Name fragment of a retired WAL generation capture directory beside its database
const RetiredGenerationSuffix = ".retired-wal-"

// File a capture directory describes itself in
const retiredManifest = "manifest.json"

// Format version of the capture manifest
const retiredManifestVersion = 1

// Largest main image a capture copies whole, larger ones keeping only the header
const retiredMainImageMax = 512 * 1024 * 1024

// Bytes one copy step reads
const captureChunk = 8 * 1024 * 1024

// Note every capture manifest carries for the operator deciding the frames' fate
const retiredNote = "Frames in the captured WAL were committed by the retired generation. Whether they " +
	"belong on top of the main file now at the path is an operator decision; inspect " +
	"the copied image with `icebath sessions recover --inspect-only` first."

// Identity as the manifest records it, device then inode
func identityList(id fileIdentity) []uint64 { return []uint64{id.dev, id.ino} }

// Streams size bytes from read into dest through a temporary part file with fsync, refusing a short copy
func copyRange(read func(off int64, n int) ([]byte, error), dest string, size int64) (map[string]any, error) {
	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	var off int64
	for off < size {
		chunk, err := read(off, int(min(int64(captureChunk), size-off)))
		if err != nil {
			return nil, joinRemove(err, out, part)
		}
		if len(chunk) == 0 {
			break
		}
		if _, err := out.Write(chunk); err != nil {
			return nil, joinRemove(err, out, part)
		}
		digest.Write(chunk)
		off += int64(len(chunk))
	}
	if err := out.Sync(); err != nil {
		return nil, joinRemove(err, out, part)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(part)
		return nil, err
	}
	if off != size {
		_ = os.Remove(part)
		return nil, &CaptureError{msg: fmt.Sprintf("short read copying %s: %d of %d bytes — the retired generation "+
			"is NOT fully captured; the handle stays open for a retry", filepath.Base(dest), off, size)}
	}
	if err := os.Rename(part, dest); err != nil {
		return nil, err
	}
	return map[string]any{"file": filepath.Base(dest), "bytes": off, "sha256": hex.EncodeToString(digest.Sum(nil))}, nil
}

// Closes and removes a part file after a failed copy, returning the copy error
func joinRemove(err error, f *os.File, part string) error {
	_ = f.Close()
	_ = os.Remove(part)
	return err
}

// Capture directory name for path, the UTC instant and pid naming it
func captureStem(path string, now time.Time) string {
	return fmt.Sprintf("%s%s%s-%d", filepath.Base(path), RetiredGenerationSuffix, now.UTC().Format("20060102-150405"), os.Getpid())
}
