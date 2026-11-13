package state

import (
	"bytes"
	"encoding/binary"
	"os"
)

// Header layout of a SQLite database file
const (
	sqliteMagic         = "SQLite format 3\x00"
	sqliteHeaderBytes   = 100
	applicationIDOffset = 68
	headerProbeBytes    = 100
)

// Application id the database header stores, read through the lock-safe probe, false without a valid header
func (e *Engine) readApplicationID(path string) (uint32, bool) {
	end := applicationIDOffset + 4
	head, ok := e.preadDB(path, 0, end)
	if !ok || len(head) < end || string(head[:16]) != sqliteMagic {
		return 0, false
	}
	return binary.BigEndian.Uint32(head[applicationIDOffset:end]), true
}

// First probe bytes of a regular file read without a SQLite connection, refused while one is live unless forced
func (e *Engine) preopenHeader(path string, probe int, force bool) ([]byte, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	if !force && e.HasLiveConnection(path) {
		return nil, false
	}
	return e.ReadHeader(path, max(16, probe), force)
}

// Reports a file whose first page is not SQLite at all, empty, zeroed or overwritten
func (e *Engine) hasInvalidHeader(path string, force bool) bool {
	head, ok := e.preopenHeader(path, headerProbeBytes, force)
	return ok && !bytes.HasPrefix(head, []byte("SQLite format 3"))
}

// Reports the zeroed signature, an empty file or a header of NUL bytes only
func (e *Engine) isZeroedFile(path string, force bool) bool {
	head, ok := e.preopenHeader(path, headerProbeBytes, force)
	return ok && !bytes.HasPrefix(head, []byte("SQLite format 3")) && bytes.Count(head, []byte{0}) == len(head)
}

// Header fields a retired generation manifest records
type headerFields struct {
	Valid           bool    `json:"valid"`
	PageSize        *uint32 `json:"page_size,omitempty"`
	ChangeCounter   *uint32 `json:"change_counter,omitempty"`
	PageCount       *uint32 `json:"page_count,omitempty"`
	UserVersion     *uint32 `json:"user_version,omitempty"`
	ApplicationID   *uint32 `json:"application_id,omitempty"`
	VersionValidFor *uint32 `json:"version_valid_for,omitempty"`
}

// Parsed fields of a SQLite header, valid false for anything that is not one
func parseHeader(head []byte) headerFields {
	if len(head) < sqliteHeaderBytes || string(head[:16]) != sqliteMagic {
		return headerFields{}
	}
	field := func(off int) *uint32 { v := binary.BigEndian.Uint32(head[off : off+4]); return &v }
	size := uint32(binary.BigEndian.Uint16(head[16:18]))
	if size == 1 {
		size = 65536
	}
	return headerFields{
		Valid: true, PageSize: &size, ChangeCounter: field(24), PageCount: field(28), UserVersion: field(60),
		ApplicationID: field(applicationIDOffset), VersionValidFor: field(92),
	}
}
