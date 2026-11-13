package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	MaxRoomAttachmentBytes       = 512 * 1024 * 1024
	MaxGatewayBlobBytes          = 2 * 1024 * 1024 * 1024
	MaxRoomAttachmentCount       = 50_000
	MaxGatewayAttachmentCount    = 200_000
	MaxRoomUncommittedBytes      = 100 * 1024 * 1024
	MaxRoomUncommittedCount      = 64
	UncommittedTTLSeconds        = 60 * 60
	DisbandedGraceSeconds        = 15 * 60
	ClassicAttachmentTTLSeconds  = 7 * 24 * 60 * 60
	maxAttachmentIdentifierChars = 128
)

var blobIDRe = regexp.MustCompile(`^blob_[0-9a-f]{32}$`)

// Attachment id unknown, expired or unavailable to the caller
type AttachmentNotFoundError struct{ *AttachmentError }

// Idempotency or ownership key reused with different content
type AttachmentConflictError struct{ *AttachmentError }

// Bounded room or gateway quota would be exceeded
type AttachmentQuotaError struct{ *AttachmentError }

// Canonical blob bytes no longer match their durable metadata
type AttachmentIntegrityError struct{ *AttachmentError }

func (e *AttachmentNotFoundError) Unwrap() error  { return e.AttachmentError }
func (e *AttachmentConflictError) Unwrap() error  { return e.AttachmentError }
func (e *AttachmentQuotaError) Unwrap() error     { return e.AttachmentError }
func (e *AttachmentIntegrityError) Unwrap() error { return e.AttachmentError }

func attachmentErr(msg string) error { return &AttachmentError{Msg: msg} }
func attachmentNotFound(msg string) error {
	return &AttachmentNotFoundError{&AttachmentError{Msg: msg}}
}
func attachmentConflict(msg string) error {
	return &AttachmentConflictError{&AttachmentError{Msg: msg}}
}
func attachmentQuota(msg string) error { return &AttachmentQuotaError{&AttachmentError{Msg: msg}} }
func attachmentIntegrity(msg string) error {
	return &AttachmentIntegrityError{&AttachmentError{Msg: msg}}
}

// Verified canonical bytes plus safe attachment metadata
type AttachmentData struct {
	Attachment map[string]any
	Data       []byte
}

// Private byte store root beside the room state database
func DefaultAttachmentRoot(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "hosted-room-attachments")
}

func attachmentIdentifier(value any, label string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", attachmentErr(label + " must be a string")
	}
	normalized := pystr.Strip(text)
	if normalized == "" || utf8.RuneCountInString(normalized) > maxAttachmentIdentifierChars || !roomIdentifierRe.MatchString(normalized) {
		return "", attachmentErr("invalid " + label)
	}
	return normalized, nil
}

func attachmentIDOf(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !attachmentIDRe.MatchString(text) {
		return "", attachmentErr("invalid attachment_id")
	}
	return text, nil
}

func attachmentName(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", attachmentErr("attachment name must be a string")
	}
	normalized := pystr.Strip(text)
	if !utf8.ValidString(normalized) {
		return "", attachmentErr("attachment name must contain valid Unicode")
	}
	if normalized == "" || utf8.RuneCountInString(normalized) > maxAttachmentNameChars || normalized == "." || normalized == ".." ||
		strings.ContainsAny(normalized, "/\\\x00\n\r") {
		return "", attachmentErr("attachment name must be a bounded basename")
	}
	return normalized, nil
}

func attachmentMime(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", attachmentErr("attachment mime must be a string")
	}
	normalized := pystr.Lower(pystr.Strip(text))
	if normalized == "" || utf8.RuneCountInString(normalized) > maxAttachmentMimeChars || !attachmentMimeRe.MatchString(normalized) {
		return "", attachmentErr("attachment mime is invalid")
	}
	return normalized, nil
}

func attachmentKind(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !attachmentKinds[text] {
		return "", attachmentErr("attachment kind must be image, pdf, or file")
	}
	return text, nil
}

// MIME type the leading bytes prove, octet stream when nothing matches
func sniffKnownMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(data, []byte("BM")):
		return "image/bmp"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return "application/zip"
	case bytes.HasPrefix(data, []byte("\x1f\x8b")):
		return "application/gzip"
	}
	if !bytes.Contains(data, []byte{0}) && utf8.Valid(data) {
		return "text/plain"
	}
	return "application/octet-stream"
}

func validateMimeAndKind(data []byte, kind, mime string) error {
	sniffed := sniffKnownMime(data)
	switch kind {
	case "image":
		if !strings.HasPrefix(mime, "image/") || sniffed != mime {
			return attachmentErr("image bytes do not match the declared MIME type")
		}
		return nil
	case "pdf":
		if mime != "application/pdf" || sniffed != "application/pdf" {
			return attachmentErr("PDF bytes do not match application/pdf")
		}
		return nil
	}
	if strings.HasPrefix(mime, "image/") || mime == "application/pdf" {
		return attachmentErr("image and PDF MIME types require their matching kind")
	}
	if strings.HasPrefix(mime, "text/") && sniffed != "text/plain" {
		return attachmentErr("text bytes do not match the declared MIME type")
	}
	return nil
}

// Strictly decoded bounded RPC upload body, data URLs refused
func DecodeContentBase64(value any) ([]byte, error) {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil, attachmentErr("content_base64 is required")
	}
	if strings.HasPrefix(pystr.Lower(pystr.Lstrip(text)), "data:") {
		return nil, attachmentErr("data URLs are not accepted")
	}
	maximumEncoded := ((maxAttachmentBytes + 2) / 3) * 4
	if utf8.RuneCountInString(text) > maximumEncoded+4 {
		return nil, attachmentErr("attachment exceeds the per-file size limit")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(text)
	if err != nil {
		return nil, attachmentErr("content_base64 is not valid base64")
	}
	if len(data) == 0 {
		return nil, attachmentErr("attachment must not be empty")
	}
	if len(data) > maxAttachmentBytes {
		return nil, attachmentErr("attachment exceeds the per-file size limit")
	}
	return data, nil
}

// Base64 of verified bytes for the bounded read RPC
func EncodeContentBase64(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// Case and accent folded text of a catalog name
func FoldCatalogText(value string) string {
	ascii := true
	for i := 0; i < len(value); i++ {
		ascii = ascii && value[i] < 0x80
	}
	if ascii {
		return strings.ToLower(value)
	}
	var out strings.Builder
	for _, r := range pystr.Casefold(norm.NFKD.String(value)) {
		if norm.NFD.PropertiesString(string(r)).CCC() == 0 {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// One database file whose store was initialized, reconciled and pruned this process
type startedAttachmentStore struct {
	dbPath, root string
	info         os.FileInfo
}

var (
	attachmentStoresStarted []startedAttachmentStore
	// Set once a store whose database could not be stat finished startup, the None key of the set
	attachmentStoreStartedUnkeyed bool
	attachmentStoresStartedMu     sync.Mutex
)

// SQLite owned metadata and private content deduplicated blob bytes
type HostedRoomAttachmentStore struct {
	ctx               context.Context
	DBPath            string
	Root              string
	BlobRoot          string
	Clock             func() float64
	RoomQuotaBytes    int64
	GatewayQuotaBytes int64
	RoomQuotaCount    int64
	GatewayQuotaCount int64
	lock              pythread.RLock
}

// Store options, zero quotas meaning the module defaults
type AttachmentStoreOptions struct {
	Root              string
	Clock             func() float64
	RoomQuotaBytes    *int64
	GatewayQuotaBytes *int64
	RoomQuotaCount    *int64
	GatewayQuotaCount *int64
}

func quotaOr(value *int64, def int64) int64 {
	if value == nil {
		return def
	}
	return max(1, *value)
}

// Opens the store, initializing, reconciling and pruning once per database file
func NewHostedRoomAttachmentStore(ctx context.Context, dbPath string, o AttachmentStoreOptions) (*HostedRoomAttachmentStore, error) {
	root := o.Root
	if root == "" {
		root = DefaultAttachmentRoot(dbPath)
	}
	clock := o.Clock
	if clock == nil {
		clock = hostedWallClock
	}
	s := &HostedRoomAttachmentStore{ctx: ctx, DBPath: dbPath, Root: root, BlobRoot: filepath.Join(root, "blobs"), Clock: clock,
		RoomQuotaBytes: quotaOr(o.RoomQuotaBytes, MaxRoomAttachmentBytes), GatewayQuotaBytes: quotaOr(o.GatewayQuotaBytes, MaxGatewayBlobBytes),
		RoomQuotaCount: quotaOr(o.RoomQuotaCount, MaxRoomAttachmentCount), GatewayQuotaCount: quotaOr(o.GatewayQuotaCount, MaxGatewayAttachmentCount)}
	if err := s.preparePrivateRoot(); err != nil {
		return nil, err
	}
	attachmentStoresStartedMu.Lock()
	defer attachmentStoresStartedMu.Unlock()
	if key, keyed := s.startupKey(); keyed {
		for _, started := range attachmentStoresStarted {
			if started.dbPath == key.dbPath && started.root == key.root && os.SameFile(started.info, key.info) {
				return s, nil
			}
		}
	} else if attachmentStoreStartedUnkeyed {
		return s, nil
	}
	conn, err := s.connect()
	if err != nil {
		return nil, err
	}
	if err := state.Transaction(ctx, conn, true, s.initialize); err != nil {
		return nil, err
	}
	if _, err := s.ReconcileRoomEvents(); err != nil {
		return nil, err
	}
	if _, err := s.Prune(nil); err != nil {
		return nil, err
	}
	if key, keyed := s.startupKey(); keyed {
		attachmentStoresStarted = append(attachmentStoresStarted, key)
	} else {
		attachmentStoreStartedUnkeyed = true
	}
	return s, nil
}

// Identity of the database file, so a recreated database is set up again
func (s *HostedRoomAttachmentStore) startupKey() (startedAttachmentStore, bool) {
	info, err := os.Stat(s.DBPath)
	if err != nil {
		return startedAttachmentStore{}, false
	}
	return startedAttachmentStore{dbPath: resolvedOr(s.DBPath), root: resolvedOr(s.Root), info: info}, true
}

func resolvedOr(path string) string {
	if resolved, err := pypath.Resolve(path); err == nil {
		return resolved
	}
	return path
}

func (s *HostedRoomAttachmentStore) preparePrivateRoot() error {
	for _, path := range []string{s.Root, s.BlobRoot} {
		if _, err := home.MakeUserDir(s.ctx, path, 0o700, true, true); err != nil {
			return err
		}
	}
	for _, path := range []string{s.Root, s.BlobRoot} {
		_ = os.Chmod(path, 0o700)
	}
	return nil
}

func (s *HostedRoomAttachmentStore) initialize(conn state.SQLiteConn) error {
	ctx := s.ctx
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS hosted_room_attachment_blobs (
                blob_id TEXT PRIMARY KEY,
                sha256 TEXT NOT NULL UNIQUE,
                size INTEGER NOT NULL CHECK (size > 0),
                ref_count INTEGER NOT NULL CHECK (ref_count > 0),
                created_at REAL NOT NULL
            )`,
		`CREATE TABLE IF NOT EXISTS hosted_room_attachments (
                attachment_id TEXT PRIMARY KEY,
                upload_id TEXT NOT NULL,
                room_id TEXT NOT NULL,
                event_id TEXT,
                kind TEXT NOT NULL,
                name TEXT NOT NULL,
                size INTEGER NOT NULL CHECK (size > 0),
                mime TEXT NOT NULL,
                sha256 TEXT NOT NULL,
                blob_id TEXT NOT NULL,
                recipient_member_ids_json TEXT NOT NULL DEFAULT '[]',
                viewer_access INTEGER NOT NULL DEFAULT 0 CHECK (viewer_access IN (0, 1)),
                state TEXT NOT NULL CHECK (state IN ('uploaded', 'committed', 'disbanded')),
                created_at REAL NOT NULL,
                updated_at REAL NOT NULL,
                expires_at REAL,
                UNIQUE (room_id, upload_id),
                FOREIGN KEY (blob_id) REFERENCES hosted_room_attachment_blobs(blob_id)
            )`,
	} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	rows, err := state.QueryObjects(ctx, conn, "PRAGMA table_info(hosted_room_attachments)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for _, row := range rows {
		columns[hrStr(row, "name")] = true
	}
	if !columns["viewer_access"] {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE hosted_room_attachments
                   ADD COLUMN viewer_access INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !columns["catalog_name"] {
		if _, err := conn.ExecContext(ctx, "ALTER TABLE hosted_room_attachments ADD COLUMN catalog_name TEXT"); err != nil {
			return err
		}
		named, err := state.QueryObjects(ctx, conn, "SELECT attachment_id, name FROM hosted_room_attachments")
		if err != nil {
			return err
		}
		for _, row := range named {
			if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_attachments SET catalog_name=? WHERE attachment_id=?",
				FoldCatalogText(pyrepr.Str(row.Vals["name"])), row.Vals["attachment_id"]); err != nil {
				return err
			}
		}
	}
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_hosted_room_attachments_room_state
               ON hosted_room_attachments(room_id, state, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_hosted_room_attachments_expiry
               ON hosted_room_attachments(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_hosted_room_attachments_event
               ON hosted_room_attachments(room_id, event_id)`,
	} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

func (s *HostedRoomAttachmentStore) connect() (state.SQLiteConn, error) {
	if err := os.MkdirAll(filepath.Dir(s.DBPath), 0o777); err != nil {
		return nil, err
	}
	return state.OpenDB(s.ctx, s.DBPath, "state.db (hosted room attachments)", state.WithBusyTimeoutMS(10_000), state.WithForeignKeys())
}

func (s *HostedRoomAttachmentStore) transaction(immediate bool, body func(conn state.SQLiteConn) error) error {
	conn, err := s.connect()
	if err != nil {
		return err
	}
	return state.Transaction(s.ctx, conn, immediate, body)
}

func (s *HostedRoomAttachmentStore) blobPath(blobID string) (string, error) {
	if !blobIDRe.MatchString(blobID) {
		return "", attachmentIntegrity("stored blob id is invalid")
	}
	return filepath.Join(s.BlobRoot, blobID), nil
}

func (s *HostedRoomAttachmentStore) writeBlob(target string, data []byte) error {
	mode := os.FileMode(0o600)
	return pyfs.AtomicWriteBytes(s.ctx, target, data, ".tmp-", &mode, true)
}

// Regular blob file fenced against the durable row's size
func (s *HostedRoomAttachmentStore) openBlob(blobID string, size int64) (*os.File, error) {
	path, err := s.blobPath(blobID)
	if err != nil {
		return nil, err
	}
	handle, err := openBlobNoFollow(path)
	if err != nil {
		return nil, &AttachmentIntegrityError{&AttachmentError{Msg: "canonical attachment blob is unavailable"}}
	}
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		handle.Close()
		if err != nil {
			return nil, err
		}
		return nil, attachmentIntegrity("canonical attachment blob size changed")
	}
	return handle, nil
}

func (s *HostedRoomAttachmentStore) readBlob(blobID string, size int64, digest string) ([]byte, error) {
	handle, err := s.openBlob(blobID, size)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(handle, maxAttachmentBytes+1))
	handle.Close()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || hex.EncodeToString(sum[:]) != digest {
		return nil, attachmentIntegrity("canonical attachment blob failed SHA-256 validation")
	}
	return data, nil
}

// One slice of the blob, the receiver verifying the reassembled whole file digest
func (s *HostedRoomAttachmentStore) readBlobRange(blobID string, size, offset, length int64) ([]byte, error) {
	handle, err := s.openBlob(blobID, size)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	if _, err := handle.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(handle, length))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != min(length, size-offset) {
		return nil, attachmentIntegrity("canonical attachment blob size changed")
	}
	return data, nil
}

func attachmentMetadata(row *pyjson.Object, idempotent bool) map[string]any {
	result := map[string]any{"attachment_id": hrStr(row, "attachment_id"), "kind": hrStr(row, "kind"), "name": hrStr(row, "name"),
		"size": hrInt(row, "size"), "mime": hrStr(row, "mime"), "sha256": hrStr(row, "sha256"), "state": hrStr(row, "state"),
		"created_at": hrFloat(row, "created_at"), "idempotent": idempotent}
	if row.Vals["event_id"] != nil {
		result["event_id"] = hrStr(row, "event_id")
	}
	return result
}

func randomHex16() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *HostedRoomAttachmentStore) now() float64 { return s.Clock() }

// Stores one upload's bytes deduplicated by content, an identical retry returning the original
func (s *HostedRoomAttachmentStore) Put(roomIDValue, uploadIDValue, kindValue, nameValue, mimeValue any, data []byte) (map[string]any, error) {
	roomID, err := attachmentIdentifier(roomIDValue, "room_id")
	if err != nil {
		return nil, err
	}
	uploadID, err := attachmentIdentifier(uploadIDValue, "upload_id")
	if err != nil {
		return nil, err
	}
	kind, err := attachmentKind(kindValue)
	if err != nil {
		return nil, err
	}
	name, err := attachmentName(nameValue)
	if err != nil {
		return nil, err
	}
	mime, err := attachmentMime(mimeValue)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, attachmentErr("attachment bytes must not be empty")
	}
	if len(data) > maxAttachmentBytes {
		return nil, attachmentErr("attachment exceeds the per-file size limit")
	}
	if err := validateMimeAndKind(data, kind, mime); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	now := s.now()
	if _, err := s.Prune(&now); err != nil {
		return nil, err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	size := int64(len(data))
	var out map[string]any
	err = s.transaction(true, func(conn state.SQLiteConn) error {
		ctx := s.ctx
		existing, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_attachments WHERE room_id=? AND upload_id=?", roomID, uploadID)
		if err != nil {
			return err
		}
		if existing != nil {
			if hrStr(existing, "kind") != kind || hrStr(existing, "name") != name || hrStr(existing, "mime") != mime ||
				hrInt(existing, "size") != size || hrStr(existing, "sha256") != digest {
				return attachmentConflict("upload_id was already used for different attachment content")
			}
			if _, err := s.readBlob(hrStr(existing, "blob_id"), hrInt(existing, "size"), hrStr(existing, "sha256")); err != nil {
				return err
			}
			out = attachmentMetadata(existing, true)
			return nil
		}
		roomTotals, err := state.QueryRowObject(ctx, conn, `SELECT COALESCE(SUM(size), 0) AS bytes, COUNT(*) AS count
                     FROM hosted_room_attachments
                    WHERE room_id=? AND state!='disbanded'`, roomID)
		if err != nil {
			return err
		}
		uncommitted, err := state.QueryRowObject(ctx, conn, `SELECT COALESCE(SUM(size), 0) AS bytes, COUNT(*) AS count
                     FROM hosted_room_attachments
                    WHERE room_id=? AND state='uploaded'`, roomID)
		if err != nil {
			return err
		}
		if hrInt(roomTotals, "bytes")+size > s.RoomQuotaBytes {
			return attachmentQuota("room attachment quota exceeded")
		}
		if hrInt(roomTotals, "count") >= s.RoomQuotaCount {
			return attachmentQuota("room attachment count quota exceeded")
		}
		total, err := state.QueryRowObject(ctx, conn, "SELECT COUNT(*) AS n FROM hosted_room_attachments")
		if err != nil {
			return err
		}
		if hrInt(total, "n") >= s.GatewayQuotaCount {
			return attachmentQuota("gateway attachment count quota exceeded")
		}
		if hrInt(uncommitted, "bytes")+size > MaxRoomUncommittedBytes || hrInt(uncommitted, "count")+1 > MaxRoomUncommittedCount {
			return attachmentQuota("room uncommitted upload quota exceeded")
		}
		blob, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_attachment_blobs WHERE sha256=?", digest)
		if err != nil {
			return err
		}
		var blobID string
		if blob == nil {
			physical, err := state.QueryRowObject(ctx, conn, "SELECT COALESCE(SUM(size), 0) AS n FROM hosted_room_attachment_blobs")
			if err != nil {
				return err
			}
			if hrInt(physical, "n")+size > s.GatewayQuotaBytes {
				return attachmentQuota("gateway attachment quota exceeded")
			}
			blobID = "blob_" + randomHex16()
			target, err := s.blobPath(blobID)
			if err != nil {
				return err
			}
			if err := s.writeBlob(target, data); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_attachment_blobs
                       (blob_id, sha256, size, ref_count, created_at)
                       VALUES (?, ?, ?, 1, ?)`, blobID, digest, size, now); err != nil {
				return err
			}
		} else {
			blobID = hrStr(blob, "blob_id")
			if _, err := s.readBlob(blobID, hrInt(blob, "size"), digest); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_attachment_blobs SET ref_count=ref_count+1 WHERE blob_id=?", blobID); err != nil {
				return err
			}
		}
		attachmentID := "att_" + randomHex16()
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_attachments
                   (attachment_id, upload_id, room_id, event_id, kind, name, catalog_name, size,
                    mime, sha256, blob_id, recipient_member_ids_json, state,
                    created_at, updated_at, expires_at)
                   VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, '[]', 'uploaded', ?, ?, ?)`,
			attachmentID, uploadID, roomID, kind, name, FoldCatalogText(name), size, mime, digest, blobID, now, now, now+UncommittedTTLSeconds); err != nil {
			return err
		}
		row, err := state.QueryRowObject(ctx, conn, hostedAttachmentSelectByID, attachmentID)
		if err != nil {
			return err
		}
		if row == nil {
			return errors.New("stored attachment could not be reloaded")
		}
		out = attachmentMetadata(row, false)
		return nil
	})
	return out, err
}

// Verified metadata of an idempotent upload retry, nil when none is live
func (s *HostedRoomAttachmentStore) FindUpload(roomIDValue, uploadIDValue any) (map[string]any, error) {
	roomID, err := attachmentIdentifier(roomIDValue, "room_id")
	if err != nil {
		return nil, err
	}
	uploadID, err := attachmentIdentifier(uploadIDValue, "upload_id")
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out map[string]any
	err = s.transaction(false, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(s.ctx, conn, `SELECT * FROM hosted_room_attachments
                    WHERE room_id=? AND upload_id=?`, roomID, uploadID)
		if err != nil || row == nil || hrStr(row, "state") == "disbanded" {
			return err
		}
		if row.Vals["expires_at"] != nil && hrFloat(row, "expires_at") <= now {
			return nil
		}
		if _, err := s.readBlob(hrStr(row, "blob_id"), hrInt(row, "size"), hrStr(row, "sha256")); err != nil {
			return err
		}
		out = attachmentMetadata(row, true)
		return nil
	})
	return out, err
}

// Commit options of one message manifest
type AttachmentCommit struct {
	RoomID             any
	EventID            any
	Manifest           any
	RecipientMemberIDs []string
	ViewerAccess       bool
	RetentionSeconds   *float64
	HoldUntilEvent     bool
}

// Commits a manifest, the normalized entries only
func (s *HostedRoomAttachmentStore) CommitMessage(c AttachmentCommit) ([]map[string]any, error) {
	normalized, _, err := s.CommitMessageWithReceipt(c)
	return normalized, err
}

// Commits a manifest and reports only the rows this call transitioned
func (s *HostedRoomAttachmentStore) CommitMessageWithReceipt(c AttachmentCommit) ([]map[string]any, []string, error) {
	roomID, err := attachmentIdentifier(c.RoomID, "room_id")
	if err != nil {
		return nil, nil, err
	}
	eventID, err := attachmentIdentifier(c.EventID, "event_id")
	if err != nil {
		return nil, nil, err
	}
	normalized, err := ValidateAttachmentManifest(c.Manifest)
	if err != nil {
		return nil, nil, err
	}
	recipients := make([]string, 0, len(c.RecipientMemberIDs))
	seen := map[string]bool{}
	for _, member := range c.RecipientMemberIDs {
		id, err := attachmentIdentifier(member, "recipient_member_id")
		if err != nil {
			return nil, nil, err
		}
		recipients = append(recipients, id)
		seen[id] = true
	}
	if len(recipients) == 0 {
		return nil, nil, attachmentErr("attachment commitment requires recipient ids")
	}
	if len(seen) != len(recipients) {
		return nil, nil, attachmentErr("recipient ids must be unique")
	}
	sorted := append([]string(nil), recipients...)
	sort.Strings(sorted)
	recipientsJSON, err := pyjson.Dumps(sorted, pyjson.Separators(",", ":"))
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	var expiresAt any
	switch {
	case c.HoldUntilEvent:
		expiresAt = now + UncommittedTTLSeconds
	case c.RetentionSeconds != nil:
		expiresAt = now + *c.RetentionSeconds
	}
	viewer := int64(0)
	if c.ViewerAccess {
		viewer = 1
	}
	transitioned := []string{}
	s.lock.Lock()
	defer s.lock.Unlock()
	err = s.transaction(true, func(conn state.SQLiteConn) error {
		ctx := s.ctx
		for _, entry := range normalized {
			row, err := state.QueryRowObject(ctx, conn, hostedAttachmentSelectByID, entry["attachment_id"])
			if err != nil {
				return err
			}
			if row == nil || hrStr(row, "room_id") != roomID {
				return attachmentNotFound("attachment is not uploaded for this room")
			}
			durable := map[string]any{"attachment_id": hrStr(row, "attachment_id"), "kind": hrStr(row, "kind"), "name": hrStr(row, "name"),
				"size": hrInt(row, "size"), "mime": hrStr(row, "mime")}
			if !pyjson.Equal(durable, entry) {
				return attachmentConflict("attachment manifest metadata does not match the uploaded bytes")
			}
			if row.Vals["expires_at"] != nil && hrFloat(row, "expires_at") <= now {
				return attachmentNotFound("attachment has expired")
			}
			rowState := hrStr(row, "state")
			if rowState == "disbanded" {
				return attachmentNotFound("attachment belongs to a disbanded room")
			}
			if rowState == "committed" && (hostedStrOrEmpty(row.Vals["event_id"]) != eventID || hrStr(row, "recipient_member_ids_json") != recipientsJSON ||
				pyrepr.Truthy(row.Vals["viewer_access"]) != c.ViewerAccess) {
				return attachmentConflict("attachment is already owned by a different room event")
			}
			if rowState == "uploaded" {
				transitioned = append(transitioned, pyrepr.Str(entry["attachment_id"]))
			}
			if _, err := s.readBlob(hrStr(row, "blob_id"), hrInt(row, "size"), hrStr(row, "sha256")); err != nil {
				return err
			}
		}
		for _, entry := range normalized {
			id := pyrepr.Str(entry["attachment_id"])
			found := false
			for _, t := range transitioned {
				found = found || t == id
			}
			if !found {
				continue
			}
			result, err := conn.ExecContext(ctx, `UPDATE hosted_room_attachments
                          SET event_id=?, recipient_member_ids_json=?, viewer_access=?, state='committed',
                              updated_at=?, expires_at=?
                        WHERE attachment_id=? AND state='uploaded'`, eventID, recipientsJSON, viewer, now, expiresAt, id)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return attachmentConflict("attachment changed during message commitment")
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return normalized, transitioned, nil
}

// Returns a failed pre event commitment to its bounded upload state
func (s *HostedRoomAttachmentStore) AbortMessageCommit(roomIDValue, eventIDValue any, attachmentIDs []string) (int64, error) {
	roomID, err := attachmentIdentifier(roomIDValue, "room_id")
	if err != nil {
		return 0, err
	}
	eventID, err := attachmentIdentifier(eventIDValue, "event_id")
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for _, value := range attachmentIDs {
		id, err := attachmentIDOf(value)
		if err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	now := s.now()
	var changed int64
	err = s.transaction(true, func(conn state.SQLiteConn) error {
		owner, err := attachmentOwnerEvent(s.ctx, conn, roomID, eventID)
		if err != nil {
			return err
		}
		if owner != nil {
			published, err := publishedAttachmentIDs(hrStr(owner, "payload_json"))
			if err != nil {
				return err
			}
			kept := []string{}
			for _, id := range ids {
				if !published[id] {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		if len(ids) == 0 {
			return nil
		}
		args := []any{now, now + UncommittedTTLSeconds, roomID, eventID}
		for _, id := range ids {
			args = append(args, id)
		}
		result, err := conn.ExecContext(s.ctx, `UPDATE hosted_room_attachments
                        SET event_id=NULL, recipient_member_ids_json='[]', viewer_access=0,
                            state='uploaded', updated_at=?, expires_at=?
                      WHERE room_id=? AND event_id=? AND state='committed'
                        AND attachment_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	return changed, err
}

// Attachment ids an owner payload publishes
func publishedAttachmentIDs(payloadJSON string) (map[string]bool, error) {
	payload, err := roomLoads(payloadJSON)
	if err != nil {
		return nil, err
	}
	mapping, _ := payload.(map[string]any)
	out := map[string]bool{}
	items, _ := mapping["attachments"].([]any)
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out[pyrepr.Str(m["attachment_id"])] = true
		}
	}
	return out, nil
}

// Retains committed blobs once their immutable room event is durable
func (s *HostedRoomAttachmentStore) RetainEvent(roomIDValue, eventIDValue any) (int64, error) {
	roomID, err := attachmentIdentifier(roomIDValue, "room_id")
	if err != nil {
		return 0, err
	}
	eventID, err := attachmentIdentifier(eventIDValue, "event_id")
	if err != nil {
		return 0, err
	}
	now := s.now()
	var changed int64
	err = s.transaction(true, func(conn state.SQLiteConn) error {
		event, err := state.QueryRowObject(s.ctx, conn, `SELECT 1 AS one FROM hosted_room_events
                    WHERE room_id=? AND event_id=? LIMIT 1`, roomID, eventID)
		if err != nil {
			return err
		}
		if event == nil {
			return attachmentConflict("attachment owner event is not durable in the room log")
		}
		result, err := conn.ExecContext(s.ctx, `UPDATE hosted_room_attachments
                      SET expires_at=NULL, updated_at=?
                    WHERE room_id=? AND event_id=? AND state='committed'`, now, roomID, eventID)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	return changed, err
}

// Selection of one committed row a recipient or viewer reads
type attachmentSelection struct {
	roomID, attachmentID, recipientMemberID string
	event                                   *string
	viewer                                  bool
}

func (s *HostedRoomAttachmentStore) selection(roomIDValue, attachmentIDValue, recipientValue, eventValue any, viewer, recipientRequired bool) (attachmentSelection, error) {
	var sel attachmentSelection
	var err error
	if sel.roomID, err = attachmentIdentifier(roomIDValue, "room_id"); err != nil {
		return sel, err
	}
	if sel.attachmentID, err = attachmentIDOf(attachmentIDValue); err != nil {
		return sel, err
	}
	if recipientValue != nil || recipientRequired {
		if sel.recipientMemberID, err = attachmentIdentifier(recipientValue, "recipient_member_id"); err != nil {
			return sel, err
		}
	}
	if eventValue != nil || recipientRequired {
		event, err := attachmentIdentifier(eventValue, "event_id")
		if err != nil {
			return sel, err
		}
		sel.event = &event
	}
	sel.viewer = viewer
	return sel, nil
}

// Verified bytes of a committed attachment the recipient or viewer may read
func (s *HostedRoomAttachmentStore) Read(roomID, attachmentID, recipientMemberID, eventID any, viewer bool) (AttachmentData, error) {
	sel, err := s.selection(roomID, attachmentID, recipientMemberID, eventID, viewer, false)
	if err != nil {
		return AttachmentData{}, err
	}
	now := s.now()
	var out AttachmentData
	err = s.transaction(false, func(conn state.SQLiteConn) error {
		row, err := readCommittedAttachmentRow(s.ctx, conn, sel, now)
		if err != nil {
			return err
		}
		data, err := s.readBlob(hrStr(row, "blob_id"), hrInt(row, "size"), hrStr(row, "sha256"))
		if err != nil {
			return err
		}
		out = AttachmentData{Attachment: attachmentMetadata(row, false), Data: data}
		return nil
	})
	return out, err
}

// Metadata of a committed attachment the recipient may read for the event, without bytes
func (s *HostedRoomAttachmentStore) Describe(roomID, attachmentID, recipientMemberID, eventID any) (map[string]any, error) {
	sel, err := s.selection(roomID, attachmentID, recipientMemberID, eventID, false, true)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = s.transaction(false, func(conn state.SQLiteConn) error {
		row, err := readCommittedAttachmentRow(s.ctx, conn, sel, s.now())
		if err == nil {
			out = attachmentMetadata(row, false)
		}
		return err
	})
	return out, err
}

// One slice of a committed attachment under the same authorization as a whole read
func (s *HostedRoomAttachmentStore) ReadRange(roomID, attachmentID, recipientMemberID, eventID any, offset, length any) (AttachmentData, error) {
	sel, err := s.selection(roomID, attachmentID, recipientMemberID, eventID, false, true)
	if err != nil {
		return AttachmentData{}, err
	}
	off, offOK := offset.(int64)
	n, nOK := length.(int64)
	if !offOK || !nOK || off < 0 || n <= 0 {
		return AttachmentData{}, attachmentErr("attachment range must be non-negative integers")
	}
	var out AttachmentData
	err = s.transaction(false, func(conn state.SQLiteConn) error {
		row, err := readCommittedAttachmentRow(s.ctx, conn, sel, s.now())
		if err != nil {
			return err
		}
		if off >= hrInt(row, "size") {
			return attachmentErr("attachment range starts past the end")
		}
		data, err := s.readBlobRange(hrStr(row, "blob_id"), hrInt(row, "size"), off, n)
		if err != nil {
			return err
		}
		out = AttachmentData{Attachment: attachmentMetadata(row, false), Data: data}
		return nil
	})
	return out, err
}

// Recovers only the commitments a durable event payload names
func (s *HostedRoomAttachmentStore) ReconcileRoomEvents() (int64, error) {
	var changed int64
	now := s.now()
	err := s.transaction(true, func(conn state.SQLiteConn) error {
		ctx := s.ctx
		exists, err := hostedTableExists(ctx, conn, "hosted_room_events")
		if err != nil || !exists {
			return err
		}
		rows, err := state.QueryObjects(ctx, conn, `SELECT attachment.attachment_id, attachment.event_id,
                          event.payload_json
                     FROM hosted_room_attachments AS attachment
                     JOIN hosted_room_events AS event
                       ON event.room_id=attachment.room_id
                      AND event.event_id=attachment.event_id
                    WHERE attachment.state='committed'
                      AND attachment.expires_at IS NOT NULL`)
		if err != nil {
			return err
		}
		for _, row := range rows {
			payload, err := roomLoads(pyrepr.Str(row.Vals["payload_json"]))
			if err != nil {
				continue
			}
			mapping, ok := payload.(map[string]any)
			if !ok {
				continue
			}
			items, present := mapping["attachments"]
			if !present {
				items = []any{}
			}
			list, isList := items.([]any)
			if !isList {
				continue
			}
			ids := map[string]bool{}
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					ids[hostedStrOrEmpty(m["attachment_id"])] = true
				}
			}
			if !ids[hrStr(row, "attachment_id")] {
				continue
			}
			result, err := conn.ExecContext(ctx, `UPDATE hosted_room_attachments
                          SET expires_at=NULL, updated_at=?
                        WHERE attachment_id=? AND state='committed'`, now, hrStr(row, "attachment_id"))
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			changed += n
		}
		return nil
	})
	return changed, err
}

// Marks every live attachment of the room disbanded with the grace expiry
func (s *HostedRoomAttachmentStore) MarkRoomDisbanded(roomIDValue any) (int64, error) {
	roomID, err := attachmentIdentifier(roomIDValue, "room_id")
	if err != nil {
		return 0, err
	}
	now := s.now()
	var changed int64
	err = s.transaction(true, func(conn state.SQLiteConn) error {
		result, err := conn.ExecContext(s.ctx, `UPDATE hosted_room_attachments
                      SET state='disbanded', updated_at=?, expires_at=?
                    WHERE room_id=? AND state!='disbanded'`, now, now+DisbandedGraceSeconds, roomID)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	return changed, err
}

// Removes expired attachments and unreferenced blobs, the count of removed rows
func (s *HostedRoomAttachmentStore) Prune(now *float64) (int, error) {
	at := s.now()
	if now != nil {
		at = *now
	}
	removedBlobs := []string{}
	removed := 0
	s.lock.Lock()
	err := s.transaction(true, func(conn state.SQLiteConn) error {
		ctx := s.ctx
		hasRooms, err := hostedTableExists(ctx, conn, "hosted_rooms")
		if err != nil {
			return err
		}
		if hasRooms {
			if _, err := conn.ExecContext(ctx, `UPDATE hosted_room_attachments
                          SET state='disbanded',
                              expires_at=(
                                SELECT room.disbanded_at + ? FROM hosted_rooms AS room
                                 WHERE room.room_id=hosted_room_attachments.room_id
                              ),
                              updated_at=?
                        WHERE state='committed' AND EXISTS (
                            SELECT 1 FROM hosted_rooms AS room
                             WHERE room.room_id=hosted_room_attachments.room_id
                               AND room.disbanded_at IS NOT NULL
                        )`, DisbandedGraceSeconds, at); err != nil {
				return err
			}
		}
		rows, err := state.QueryObjects(ctx, conn, `SELECT attachment_id, blob_id FROM hosted_room_attachments
                    WHERE expires_at IS NOT NULL AND expires_at <= ?`, at)
		if err != nil {
			return err
		}
		released, order := map[string]int64{}, []string{}
		for _, row := range rows {
			blobID := hrStr(row, "blob_id")
			if _, seen := released[blobID]; !seen {
				order = append(order, blobID)
			}
			released[blobID]++
		}
		for _, row := range rows {
			if _, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_attachments WHERE attachment_id=?", hrStr(row, "attachment_id")); err != nil {
				return err
			}
			removed++
		}
		for _, blobID := range order {
			count := released[blobID]
			blob, err := state.QueryRowObject(ctx, conn, "SELECT ref_count FROM hosted_room_attachment_blobs WHERE blob_id=?", blobID)
			if err != nil {
				return err
			}
			if blob == nil {
				continue
			}
			if hrInt(blob, "ref_count") <= count {
				removedBlobs = append(removedBlobs, blobID)
			} else if _, err := conn.ExecContext(ctx, `UPDATE hosted_room_attachment_blobs
                              SET ref_count=ref_count-?
                            WHERE blob_id=?`, count, blobID); err != nil {
				return err
			}
		}
		for _, blobID := range removedBlobs {
			if _, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_attachment_blobs WHERE blob_id=?", blobID); err != nil {
				return err
			}
		}
		return nil
	})
	s.lock.Unlock()
	if err != nil {
		return 0, err
	}
	for _, blobID := range removedBlobs {
		path, err := s.blobPath(blobID)
		if err != nil {
			return 0, err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
	}
	if err := s.sweepOrphans(); err != nil {
		return 0, err
	}
	return removed, nil
}

// Quarantines staging files and unreferenced blobs inside the write transaction, then unlinks them
func (s *HostedRoomAttachmentStore) sweepOrphans() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	entries, err := os.ReadDir(s.BlobRoot)
	if err != nil {
		return nil
	}
	quarantined := []string{}
	err = s.transaction(true, func(conn state.SQLiteConn) error {
		rows, err := state.QueryObjects(s.ctx, conn, "SELECT blob_id FROM hosted_room_attachment_blobs")
		if err != nil {
			return err
		}
		live := map[string]bool{}
		for _, row := range rows {
			live[hrStr(row, "blob_id")] = true
		}
		for index, entry := range entries {
			name := entry.Name()
			if !(strings.HasPrefix(name, ".tmp-") || strings.HasPrefix(name, ".prune-") || (blobIDRe.MatchString(name) && !live[name])) {
				continue
			}
			quarantine := filepath.Join(s.BlobRoot, fmt.Sprintf(".prune-%d-%d-%d-%d", os.Getpid(), pythread.GoroutineID(), time.Now().UnixNano(), index))
			if err := os.Rename(filepath.Join(s.BlobRoot, name), quarantine); err != nil {
				continue
			}
			quarantined = append(quarantined, quarantine)
		}
		return nil
	})
	for _, path := range quarantined {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
			err = rmErr
		}
	}
	return err
}

// Attachment and blob counts and byte totals, scoped to one room when given
func (s *HostedRoomAttachmentStore) Stats(roomID *string) (map[string]int64, error) {
	var out map[string]int64
	err := s.transaction(false, func(conn state.SQLiteConn) error {
		query, args := "SELECT COUNT(*) AS count, COALESCE(SUM(size), 0) AS bytes FROM hosted_room_attachments", []any{}
		if roomID != nil {
			query, args = query+" WHERE room_id=?", []any{*roomID}
		}
		row, err := state.QueryRowObject(s.ctx, conn, query, args...)
		if err != nil {
			return err
		}
		blob, err := state.QueryRowObject(s.ctx, conn, "SELECT COUNT(*) AS count, COALESCE(SUM(size), 0) AS bytes FROM hosted_room_attachment_blobs")
		if err != nil {
			return err
		}
		out = map[string]int64{"attachments": hrInt(row, "count"), "logical_bytes": hrInt(row, "bytes"), "blobs": hrInt(blob, "count"), "physical_bytes": hrInt(blob, "bytes")}
		return nil
	})
	return out, err
}

// Viewer read of a live hosted room, its authority fenced before and after the byte read
func (s *HostedRoomAttachmentStore) ReadViewer(roomIDValue, attachmentIDValue, eventIDValue, recipientValue, gatewayValue, epochValue any) (AttachmentData, error) {
	sel, err := s.selection(roomIDValue, attachmentIDValue, recipientValue, eventIDValue, true, false)
	if err != nil {
		return AttachmentData{}, err
	}
	gatewayID, err := attachmentIdentifier(gatewayValue, "authority_gateway_id")
	if err != nil {
		return AttachmentData{}, err
	}
	epoch, isInt := hostedStrictInt(epochValue)
	if !isInt || epoch < 1 {
		return AttachmentData{}, attachmentErr("authority_epoch must be a positive integer")
	}
	fenced := func() (*pyjson.Object, error) {
		var row *pyjson.Object
		err := s.transaction(false, func(conn state.SQLiteConn) error {
			if _, err := conn.ExecContext(s.ctx, "BEGIN"); err != nil {
				return err
			}
			if err := requireViewerRoom(s.ctx, conn, sel.roomID, gatewayID, epoch); err != nil {
				return err
			}
			var err error
			row, err = readCommittedAttachmentRow(s.ctx, conn, sel, s.now())
			return err
		})
		return row, err
	}
	row, err := fenced()
	if err != nil {
		return AttachmentData{}, err
	}
	data, err := s.readBlob(hrStr(row, "blob_id"), hrInt(row, "size"), hrStr(row, "sha256"))
	if err != nil {
		return AttachmentData{}, err
	}
	current, err := fenced()
	if err != nil {
		return AttachmentData{}, err
	}
	if !pyrepr.Equal(current.Vals["blob_id"], row.Vals["blob_id"]) || !pyjson.Equal(attachmentMetadata(current, false), attachmentMetadata(row, false)) {
		return AttachmentData{}, attachmentNotFound("attachment changed during viewer read")
	}
	return AttachmentData{Attachment: attachmentMetadata(current, false), Data: data}, nil
}

func requireViewerRoom(ctx context.Context, conn state.SQLiteConn, roomID, gatewayID string, epoch int64) error {
	exists, err := hostedTableExists(ctx, conn, "hosted_rooms")
	if err != nil {
		return err
	}
	if !exists {
		return attachmentNotFound("Group Chat is unavailable to viewers")
	}
	room, err := state.QueryRowObject(ctx, conn, `SELECT authority_gateway_id, authority_epoch, disbanded_at
               FROM hosted_rooms WHERE room_id=?`, roomID)
	if err != nil {
		return err
	}
	if room == nil || room.Vals["disbanded_at"] != nil || !pyrepr.Equal(room.Vals["authority_gateway_id"], gatewayID) || !pyrepr.Equal(room.Vals["authority_epoch"], epoch) {
		return attachmentNotFound("Group Chat viewer authority changed")
	}
	for _, table := range []string{"hosted_room_quarantine", "hosted_room_disband_fences"} {
		exists, err := hostedTableExists(ctx, conn, table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		hit, err := state.QueryRowObject(ctx, conn, "SELECT 1 AS one FROM "+table+" WHERE room_id=?", roomID)
		if err != nil {
			return err
		}
		if hit != nil {
			return attachmentNotFound("Group Chat is unavailable to viewers")
		}
	}
	return nil
}

// Committed row the selection may read, room visible files requiring a published owner event
func readCommittedAttachmentRow(ctx context.Context, conn state.SQLiteConn, sel attachmentSelection, now float64) (*pyjson.Object, error) {
	row, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_attachments WHERE attachment_id=? AND room_id=?", sel.attachmentID, sel.roomID)
	if err != nil {
		return nil, err
	}
	if row == nil || hrStr(row, "state") != "committed" {
		return nil, attachmentNotFound("attachment is not committed for this room")
	}
	if row.Vals["expires_at"] != nil && hrFloat(row, "expires_at") <= now {
		return nil, attachmentNotFound("attachment has expired")
	}
	if sel.event != nil && hostedStrOrEmpty(row.Vals["event_id"]) != *sel.event {
		return nil, attachmentNotFound("attachment is not owned by this room event")
	}
	viewerAccess, err := hostedIntOrZero(row.Vals["viewer_access"])
	if err != nil {
		return nil, err
	}
	if viewerAccess != 0 {
		exists, err := hostedTableExists(ctx, conn, "hosted_room_events")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, attachmentNotFound("attachment is not published in this room")
		}
		owner, err := state.QueryRowObject(ctx, conn, `SELECT payload_json FROM hosted_room_events
                       WHERE room_id=? AND event_id=? AND kind IN ('message.user', 'message.member')`, sel.roomID, hostedStrOrEmpty(row.Vals["event_id"]))
		if err != nil {
			return nil, err
		}
		manifest := map[string]any{}
		for _, key := range []string{"attachment_id", "kind", "name", "size", "mime"} {
			manifest[key] = row.Vals[key]
		}
		var published []any
		if owner != nil {
			payload, err := roomLoads(hrStr(owner, "payload_json"))
			if err != nil {
				return nil, err
			}
			if mapping, ok := payload.(map[string]any); ok {
				published, _ = mapping["attachments"].([]any)
			}
		}
		listed := false
		for _, item := range published {
			listed = listed || pyjson.Equal(item, manifest)
		}
		if !listed {
			return nil, attachmentNotFound("attachment is not published in this room")
		}
	}
	recipients, err := roomLoads(hrStr(row, "recipient_member_ids_json"))
	if err != nil {
		return nil, err
	}
	if sel.viewer {
		if viewerAccess != 1 {
			return nil, attachmentNotFound("attachment is unavailable to Group Chat viewers")
		}
		return row, nil
	}
	listed, err := attachmentRecipientListed(recipients, sel.recipientMemberID)
	if err != nil {
		return nil, err
	}
	if !listed {
		return nil, attachmentNotFound("attachment is unavailable to this recipient")
	}
	return row, nil
}

// Python membership of a member id in the decoded recipients value, a list, mapping or string
func attachmentRecipientListed(recipients any, memberID string) (bool, error) {
	switch typed := recipients.(type) {
	case []any:
		for _, member := range typed {
			if pyjson.Equal(member, memberID) {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		_, has := typed[memberID]
		return has, nil
	case string:
		return strings.Contains(typed, memberID), nil
	}
	return false, fmt.Errorf("argument of type '%s' is not iterable", pyrepr.TypeName(recipients))
}

// Owner event row of an attachment commitment, nil without a canonical room log
func attachmentOwnerEvent(ctx context.Context, conn state.SQLiteConn, roomID, eventID string) (*pyjson.Object, error) {
	exists, err := hostedTableExists(ctx, conn, "hosted_room_events")
	if err != nil || !exists {
		return nil, err
	}
	return state.QueryRowObject(ctx, conn, "SELECT payload_json FROM hosted_room_events WHERE room_id=? AND event_id=?", roomID, eventID)
}
