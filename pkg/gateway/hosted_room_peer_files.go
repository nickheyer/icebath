package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	// Seconds a file a peer holds for a room member lives after its last use
	peerFileTTLSeconds = 24 * 60 * 60
	// Bytes this gateway holds for one room member seat before it refuses more
	maxPeerFileScopeBytes = MaxRoomUncommittedBytes
	// Files this gateway holds for one room member seat before it refuses more
	maxPeerFileScopeCount = MaxRoomUncommittedCount
	// Bytes this gateway holds for every room member seat together
	maxPeerFileGatewayBytes = MaxGatewayBlobBytes / 2
	// Raw bytes one file chunk request may carry
	maxPeerFileChunkBytes = 4 * peerFileChunkBytes
)

var (
	peerFileScopeFields = []string{"room_id", "home_install_id", "authority_gateway_id", "authority_epoch", "member_id", "target_install_id", "target_profile"}
	peerFileIDRe        = regexp.MustCompile(`^pf_[0-9a-f]{32}$`)
)

// File a room member's gateway will not take, its HTTP status, machine code and the words naming the rule
type PeerFileRefusal struct {
	Status int
	Code   string
	Msg    string
}

func (e *PeerFileRefusal) Error() string { return e.Msg }

func peerFileRefuse(status int, code, format string, args ...any) error {
	return &PeerFileRefusal{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Scope of a verified room grant, one member seat of one room home, surviving grant refreshes
func PeerFileScope(claims *jsonutil.Object) (string, error) {
	values := make([]any, 0, len(peerFileScopeFields))
	for _, field := range peerFileScopeFields {
		value, ok := claims.Get(field)
		if !ok {
			return "", &HostedKeyError{field}
		}
		values = append(values, value)
	}
	text, err := roomCompactJSON(values, true)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), nil
}

// Creates the tables of files held for room members and of reply files staged for their runs
func peerFilesSchema(ctx context.Context, conn state.SQLiteConn) error {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS hosted_room_peer_files (
            file_id TEXT PRIMARY KEY, scope TEXT NOT NULL, direction TEXT NOT NULL CHECK (direction IN ('in', 'out')),
            run_id TEXT NOT NULL DEFAULT '', ordinal INTEGER NOT NULL DEFAULT 0,
            name TEXT NOT NULL, kind TEXT NOT NULL, mime TEXT NOT NULL, size INTEGER NOT NULL CHECK (size > 0), sha256 TEXT NOT NULL,
            received INTEGER NOT NULL DEFAULT 0, complete INTEGER NOT NULL DEFAULT 0,
            created_at REAL NOT NULL, updated_at REAL NOT NULL, expires_at REAL NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_hosted_room_peer_files_in ON hosted_room_peer_files(scope, sha256) WHERE direction='in'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_hosted_room_peer_files_out ON hosted_room_peer_files(scope, run_id, ordinal) WHERE direction='out'`,
		`CREATE TABLE IF NOT EXISTS hosted_room_peer_runs (
            scope TEXT NOT NULL, run_id TEXT NOT NULL, text TEXT NOT NULL, files_json TEXT NOT NULL, expires_at REAL NOT NULL,
            PRIMARY KEY (scope, run_id))`,
	} {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return nil
}

// Runs body in one transaction over the room store with the peer file tables in place
func peerFilesTx(ctx context.Context, dbPath string, body func(conn state.SQLiteConn) error) error {
	return hostedTransaction(ctx, dbPath, true, func(conn state.SQLiteConn) error {
		if err := peerFilesSchema(ctx, conn); err != nil {
			return err
		}
		return body(conn)
	})
}

// Private path of one held file beside the room store
func peerFilePath(dbPath, fileID string) (string, error) {
	if !peerFileIDRe.MatchString(fileID) {
		return "", peerFileRefuse(http.StatusNotFound, "attachment_not_found", "this gateway holds no such file")
	}
	dir := filepath.Join(filepath.Dir(dbPath), "hosted-room-peer-files")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, fileID), nil
}

// Drops held files and staged replies past their lifetime
func prunePeerFiles(ctx context.Context, conn state.SQLiteConn, dbPath string, now float64) error {
	rows, err := state.QueryObjects(ctx, conn, "SELECT file_id FROM hosted_room_peer_files WHERE expires_at <= ?", now)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if path, err := peerFilePath(dbPath, hrStr(row, "file_id")); err == nil {
			_ = os.Remove(path)
		}
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_peer_files WHERE expires_at <= ?", now); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "DELETE FROM hosted_room_peer_runs WHERE expires_at <= ?", now)
	return err
}

// Refuses a kind and MIME pair the home gateway's type rules refuse, naming the file
func peerFileTypeRule(name, kind, mimeType string) error {
	switch {
	case kind == "image" && !strings.HasPrefix(mimeType, "image/"):
		return peerFileRefuse(http.StatusUnsupportedMediaType, "attachment_type_refused", "%s is filed as an image but its type is %s", name, mimeType)
	case kind == "pdf" && mimeType != "application/pdf":
		return peerFileRefuse(http.StatusUnsupportedMediaType, "attachment_type_refused", "%s is filed as a PDF but its type is %s", name, mimeType)
	case kind == "file" && (strings.HasPrefix(mimeType, "image/") || mimeType == "application/pdf"):
		return peerFileRefuse(http.StatusUnsupportedMediaType, "attachment_type_refused", "%s has type %s, which needs its image or PDF kind", name, mimeType)
	}
	return nil
}

// Refuses a file size over the room limit or this gateway's own inbound limit, naming the limit
func peerFileSizeRule(ctx context.Context, name string, size int64) error {
	if size > maxAttachmentBytes {
		return peerFileRefuse(http.StatusRequestEntityTooLarge, "attachment_too_large", "%s is %s, over the %s limit for one room file",
			name, roomBytes(size), roomBytes(maxAttachmentBytes))
	}
	if limit := inboundMediaLimit(ctx); limit > 0 && size > limit {
		return peerFileRefuse(http.StatusRequestEntityTooLarge, "attachment_too_large",
			"%s is %s, over the %s limit the member's gateway sets for inbound files (its gateway.max_inbound_media_bytes)", name, roomBytes(size), roomBytes(limit))
	}
	return nil
}

// Answer naming a held file and the bytes this gateway has of it
func peerFileAnswer(row *jsonutil.Object) map[string]any {
	return map[string]any{"object": "icebath.room_member.file", "file_id": hrStr(row, "file_id"), "size": hrInt(row, "size"),
		"received": hrInt(row, "received"), "complete": hrInt(row, "complete") != 0}
}

// Opens or resumes the transfer of one declared turn file, refused with the limit or rule it breaks
func BeginPeerFile(ctx context.Context, dbPath, scope string, meta *jsonutil.Object) (map[string]any, error) {
	invalid := func(err error) error {
		return peerFileRefuse(http.StatusBadRequest, "invalid_attachment", "%s", err.Error())
	}
	name, err := attachmentName(meta.ValueOr("name", nil))
	if err != nil {
		return nil, invalid(err)
	}
	kind, err := attachmentKind(meta.ValueOr("kind", nil))
	if err != nil {
		return nil, invalid(err)
	}
	mimeType, err := attachmentMime(meta.ValueOr("mime", nil))
	if err != nil {
		return nil, invalid(err)
	}
	size, ok := hostedStrictInt(meta.ValueOr("size", nil))
	if !ok || size <= 0 {
		return nil, peerFileRefuse(http.StatusBadRequest, "invalid_attachment", "%s needs a positive size", name)
	}
	digest, err := hostedDigest(meta.ValueOr("sha256", nil), "sha256")
	if err != nil {
		return nil, invalid(err)
	}
	if err := peerFileTypeRule(name, kind, mimeType); err != nil {
		return nil, err
	}
	if err := peerFileSizeRule(ctx, name, size); err != nil {
		return nil, err
	}
	now := hostedWallClock()
	var answer map[string]any
	err = peerFilesTx(ctx, dbPath, func(conn state.SQLiteConn) error {
		if err := prunePeerFiles(ctx, conn, dbPath, now); err != nil {
			return err
		}
		row, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_peer_files WHERE scope=? AND direction='in' AND sha256=?", scope, digest)
		if err != nil {
			return err
		}
		if row != nil {
			if hrInt(row, "size") != size {
				return peerFileRefuse(http.StatusBadRequest, "invalid_attachment", "%s was declared with another size before", name)
			}
			path, err := peerFilePath(dbPath, hrStr(row, "file_id"))
			if err != nil {
				return err
			}
			received, complete := hrInt(row, "received"), hrInt(row, "complete")
			if info, err := os.Stat(path); err != nil || (complete != 0 && info.Size() != size) || info.Size() < received {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					return err
				}
				received, complete = 0, 0
			} else if complete == 0 {
				if err := os.Truncate(path, received); err != nil {
					return err
				}
			}
			if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_peer_files SET received=?, complete=?, updated_at=?, expires_at=? WHERE file_id=?",
				received, complete, now, now+peerFileTTLSeconds, hrStr(row, "file_id")); err != nil {
				return err
			}
			answer = map[string]any{"object": "icebath.room_member.file", "file_id": hrStr(row, "file_id"), "size": size, "received": received,
				"complete": complete != 0}
			return nil
		}
		held, err := state.QueryRowObject(ctx, conn, `SELECT COALESCE(SUM(size), 0) AS bytes, COUNT(*) AS count
                FROM hosted_room_peer_files WHERE scope=? AND direction='in'`, scope)
		if err != nil {
			return err
		}
		if hrInt(held, "bytes")+size > maxPeerFileScopeBytes || hrInt(held, "count") >= maxPeerFileScopeCount {
			return peerFileRefuse(http.StatusInsufficientStorage, "attachment_quota",
				"%s would pass the %s and %d files the member's gateway holds for one room member", name, roomBytes(maxPeerFileScopeBytes), maxPeerFileScopeCount)
		}
		all, err := state.QueryRowObject(ctx, conn, "SELECT COALESCE(SUM(size), 0) AS bytes FROM hosted_room_peer_files")
		if err != nil {
			return err
		}
		if hrInt(all, "bytes")+size > maxPeerFileGatewayBytes {
			return peerFileRefuse(http.StatusInsufficientStorage, "attachment_quota", "%s would pass the %s the member's gateway holds for room members",
				name, roomBytes(maxPeerFileGatewayBytes))
		}
		fileID := "pf_" + randomHex16()
		path, err := peerFilePath(dbPath, fileID)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_peer_files (file_id, scope, direction, name, kind, mime, size, sha256,
                received, complete, created_at, updated_at, expires_at) VALUES (?, ?, 'in', ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)`,
			fileID, scope, name, kind, mimeType, size, digest, now, now, now+peerFileTTLSeconds); err != nil {
			return err
		}
		answer = map[string]any{"object": "icebath.room_member.file", "file_id": fileID, "size": size, "received": int64(0), "complete": false}
		return nil
	})
	return answer, err
}

// Writes one chunk at the offset this gateway reached, the whole file checked against its digest and type
func WritePeerFileChunk(ctx context.Context, dbPath, scope, fileID string, offset int64, data []byte) (map[string]any, error) {
	if len(data) == 0 || len(data) > maxPeerFileChunkBytes {
		return nil, peerFileRefuse(http.StatusBadRequest, "invalid_attachment", "a file chunk carries 1 byte to %s", roomBytes(maxPeerFileChunkBytes))
	}
	path, err := peerFilePath(dbPath, fileID)
	if err != nil {
		return nil, err
	}
	now := hostedWallClock()
	var answer map[string]any
	var refusal error
	err = peerFilesTx(ctx, dbPath, func(conn state.SQLiteConn) error {
		row, err := state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_peer_files WHERE file_id=? AND scope=? AND direction='in'", fileID, scope)
		if err != nil {
			return err
		}
		if row == nil {
			return peerFileRefuse(http.StatusNotFound, "attachment_not_found", "this gateway holds no such file")
		}
		size, received, name := hrInt(row, "size"), hrInt(row, "received"), hrStr(row, "name")
		if hrInt(row, "complete") != 0 {
			answer = peerFileAnswer(row)
			return nil
		}
		if offset < 0 || offset > received {
			return peerFileRefuse(http.StatusConflict, "attachment_offset", "this gateway holds %d bytes of %s", received, name)
		}
		end := offset + int64(len(data))
		if end > size {
			return peerFileRefuse(http.StatusBadRequest, "invalid_attachment", "the chunk passes the declared size of %s", name)
		}
		output, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := output.WriteAt(data, offset); err != nil {
			output.Close()
			return err
		}
		if err := output.Sync(); err != nil {
			output.Close()
			return err
		}
		if err := output.Close(); err != nil {
			return err
		}
		received = max(received, end)
		complete := int64(0)
		if received == size {
			whole, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(whole)
			switch {
			case int64(len(whole)) != size || hex.EncodeToString(sum[:]) != hrStr(row, "sha256"):
				refusal = peerFileRefuse(http.StatusUnprocessableEntity, "attachment_digest_mismatch", "%s arrived with another SHA-256 digest than declared, send it again", name)
			default:
				if err := validateMimeAndKind(whole, hrStr(row, "kind"), hrStr(row, "mime")); err != nil {
					refusal = peerFileRefuse(http.StatusUnsupportedMediaType, "attachment_type_refused", "%s: %s", name, err.Error())
				}
			}
			if refusal != nil {
				_ = os.Remove(path)
				_, err := conn.ExecContext(ctx, "DELETE FROM hosted_room_peer_files WHERE file_id=?", fileID)
				return err
			}
			complete = 1
		}
		if _, err := conn.ExecContext(ctx, "UPDATE hosted_room_peer_files SET received=?, complete=?, updated_at=?, expires_at=? WHERE file_id=?",
			received, complete, now, now+peerFileTTLSeconds, fileID); err != nil {
			return err
		}
		answer = map[string]any{"object": "icebath.room_member.file", "file_id": fileID, "size": size, "received": received, "complete": complete == 1}
		return nil
	})
	if err == nil && refusal != nil {
		return nil, refusal
	}
	return answer, err
}

// Verified bytes of one complete file a dispatch names, refused when it never arrived or no longer matches the item
func peerDispatchFile(ctx context.Context, dbPath, scope string, item map[string]any) ([]byte, error) {
	name := fmt.Sprint(item["name"])
	var row *jsonutil.Object
	err := peerFilesTx(ctx, dbPath, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_peer_files WHERE scope=? AND direction='in' AND sha256=? AND complete=1",
			scope, item["sha256"])
		if err != nil || row == nil {
			return err
		}
		now := hostedWallClock()
		_, err = conn.ExecContext(ctx, "UPDATE hosted_room_peer_files SET updated_at=?, expires_at=? WHERE file_id=?", now, now+peerFileTTLSeconds, hrStr(row, "file_id"))
		return err
	})
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, peerFileRefuse(http.StatusConflict, "attachment_missing", "%s did not reach this gateway before the turn", name)
	}
	size, _ := hostedStrictInt(item["size"])
	path, err := peerFilePath(dbPath, hrStr(row, "file_id"))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || hrInt(row, "size") != size || hex.EncodeToString(sum[:]) != item["sha256"] {
		return nil, peerFileRefuse(http.StatusConflict, "attachment_missing", "%s no longer matches what the room sent", name)
	}
	if err := validateMimeAndKind(data, fmt.Sprint(item["kind"]), fmt.Sprint(item["mime"])); err != nil {
		return nil, peerFileRefuse(http.StatusUnsupportedMediaType, "attachment_type_refused", "%s: %s", name, err.Error())
	}
	return data, nil
}

// Turn input of a dispatch, documents named by retained path and images as image parts like a home member
func PeerMemberTurnInput(ctx context.Context, dbPath, scope string, dispatch *HostedMemberDispatch) (any, error) {
	if len(dispatch.Attachments) == 0 {
		return dispatch.Prompt, nil
	}
	var total int64
	for _, item := range dispatch.Attachments {
		size, _ := hostedStrictInt(item["size"])
		total += size
	}
	if limit := inboundMediaLimit(ctx); limit > 0 && total > limit {
		return nil, peerFileRefuse(http.StatusRequestEntityTooLarge, "attachment_too_large",
			"the files of this turn are %s, over the %s limit the member's gateway sets for inbound files (its gateway.max_inbound_media_bytes)", roomBytes(total), roomBytes(limit))
	}
	text := dispatch.Prompt
	var images []any
	for _, item := range dispatch.Attachments {
		data, err := peerDispatchFile(ctx, dbPath, scope, item)
		if err != nil {
			return nil, err
		}
		mimeType, name := fmt.Sprint(item["mime"]), fmt.Sprint(item["name"])
		if attachmentMimes[mimeType] {
			if len(images) >= apiImageLimit {
				return nil, peerFileRefuse(http.StatusRequestEntityTooLarge, "attachment_too_large", "%s passes the %d images one turn takes", name, apiImageLimit)
			}
			images = append(images, jsonutil.ObjectOf("type", "image_url", "image_url",
				jsonutil.ObjectOf("url", "data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(data))))
			continue
		}
		reference, err := captureRoomInput(ctx, name, data)
		if err != nil {
			return nil, err
		}
		paths, err := restoreNativeMedia(ctx, []any{reference})
		if err != nil {
			return nil, err
		}
		text += HostedDocumentLine + paths[0] + "\n"
	}
	if len(images) == 0 {
		return text, nil
	}
	content := append([]any{jsonutil.ObjectOf("type", "text", "text", text)}, images...)
	return []any{jsonutil.ObjectOf("role", "user", "content", content)}, nil
}

// Reply text of a finished run without MEDIA directives, the files they name staged for the room home
func StagePeerRunFiles(ctx context.Context, dbPath, scope, runID, output string) (string, []any, error) {
	if !strings.Contains(output, "MEDIA:") {
		return output, nil, nil
	}
	now := hostedWallClock()
	var text string
	var files []any
	err := peerFilesTx(ctx, dbPath, func(conn state.SQLiteConn) error {
		staged, err := state.QueryRowObject(ctx, conn, "SELECT text, files_json FROM hosted_room_peer_runs WHERE scope=? AND run_id=?", scope, runID)
		if err != nil {
			return err
		}
		if staged != nil {
			decoded, err := roomLoads(hrStr(staged, "files_json"))
			if err != nil {
				return err
			}
			text, files = hrStr(staged, "text"), decoded.([]any)
			return nil
		}
		media, cleaned, err := ExtractMedia(ctx, output)
		if err != nil {
			return err
		}
		var dropped []map[string]any
		safe, err := FilterMediaDeliveryPathsFor(ctx, media, "", &dropped)
		if err != nil {
			return err
		}
		var notes []string
		for _, item := range dropped {
			notes = append(notes, fmt.Sprintf("[%s was not shared: %v]", roomFileName(fmt.Sprint(item["path"])), item["reason"]))
		}
		files = []any{}
		var total int64
		for index, ref := range safe {
			name := roomFileName(ref.Path)
			data, readErr := readBoundedFile(ref.Path, maxAttachmentBytes)
			switch {
			case readErr != nil:
				notes = append(notes, fmt.Sprintf("[%s was not shared: %s]", name, readErr.Error()))
				continue
			case len(files) >= maxAttachmentsPerMessage:
				notes = append(notes, fmt.Sprintf("[%s was not shared: one message brings at most %d files]", name, maxAttachmentsPerMessage))
				continue
			case total+int64(len(data)) > maxMessageAttachmentBytes:
				notes = append(notes, fmt.Sprintf("[%s was not shared: the files of one message stay under %s]", name, roomBytes(maxMessageAttachmentBytes)))
				continue
			}
			kind, mimeType := roomFileType(name, data)
			sum := sha256.Sum256(data)
			fileID := "pf_" + randomHex16()
			path, err := peerFilePath(dbPath, fileID)
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO hosted_room_peer_files (file_id, scope, direction, run_id, ordinal, name, kind, mime, size,
                    sha256, received, complete, created_at, updated_at, expires_at) VALUES (?, ?, 'out', ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
				fileID, scope, runID, index, name, kind, mimeType, len(data), hex.EncodeToString(sum[:]), len(data), now, now, now+peerFileTTLSeconds); err != nil {
				return err
			}
			files = append(files, map[string]any{"file_id": fileID, "name": name, "kind": kind, "mime": mimeType, "size": int64(len(data)),
				"sha256": hex.EncodeToString(sum[:])})
			total += int64(len(data))
		}
		text = cleaned
		if len(notes) > 0 {
			text = strings.TrimSpace(cleaned + "\n\n" + strings.Join(notes, "\n"))
		}
		filesJSON, err := roomCompactJSON(files, false)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO hosted_room_peer_runs (scope, run_id, text, files_json, expires_at) VALUES (?, ?, ?, ?, ?)",
			scope, runID, text, filesJSON, now+peerFileTTLSeconds)
		return err
	})
	return text, files, err
}

// Bytes of a regular file up to a limit, refused past it with the limit named
func readBoundedFile(path string, limit int64) ([]byte, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the file is over the %s limit for one room file", roomBytes(limit))
	}
	if len(data) == 0 {
		return nil, errors.New("the file is empty")
	}
	return data, nil
}

// One chunk of a reply file staged for this room member seat, the home checking the whole digest
func ReadPeerRunFile(ctx context.Context, dbPath, scope, fileID string, offset int64) (map[string]any, error) {
	path, err := peerFilePath(dbPath, fileID)
	if err != nil {
		return nil, err
	}
	var row *jsonutil.Object
	if err := peerFilesTx(ctx, dbPath, func(conn state.SQLiteConn) (err error) {
		row, err = state.QueryRowObject(ctx, conn, "SELECT * FROM hosted_room_peer_files WHERE file_id=? AND scope=? AND direction='out' AND complete=1", fileID, scope)
		return err
	}); err != nil {
		return nil, err
	}
	if row == nil {
		return nil, peerFileRefuse(http.StatusNotFound, "attachment_not_found", "this gateway holds no such file")
	}
	size := hrInt(row, "size")
	if offset < 0 || offset >= size {
		return nil, peerFileRefuse(http.StatusBadRequest, "invalid_attachment", "offset %d is outside the %d bytes of the file", offset, size)
	}
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	chunk := make([]byte, min(int64(peerFileChunkBytes), size-offset))
	if _, err := input.ReadAt(chunk, offset); err != nil {
		return nil, err
	}
	return map[string]any{"object": "icebath.room_member.file.chunk", "file_id": fileID, "offset": offset, "size": size,
		"sha256": hrStr(row, "sha256"), "data_base64": base64.StdEncoding.EncodeToString(chunk)}, nil
}
