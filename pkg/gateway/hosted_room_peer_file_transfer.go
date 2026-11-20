package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Raw bytes one file request carries between two gateways
const peerFileChunkBytes = 1 << 20

// Peer refusal codes that name a limit or type rule no retry changes
var peerFileRefusalCodes = map[string]bool{"attachment_too_large": true, "attachment_type_refused": true, "attachment_quota": true,
	"invalid_attachment": true}

// Peer refusal of one file, the peer's own words naming its limit or type rule
type PeerFileRefusedError struct {
	Name   string
	Size   int64
	Detail string
}

func (e *PeerFileRefusedError) Error() string {
	if strings.Contains(e.Detail, e.Name) {
		return "could not take a file: " + e.Detail
	}
	return fmt.Sprintf("could not take %s (%s): %s", e.Name, roomBytes(e.Size), e.Detail)
}

// File transfer to a peer cut off, the bytes the peer already holds kept for the retry
type PeerFileTransferError struct {
	Name       string
	Held, Size int64
	Cause      error
}

func (e *PeerFileTransferError) Error() string {
	cause := e.Cause
	for next := errors.Unwrap(cause); next != nil; next = errors.Unwrap(cause) {
		cause = next
	}
	text := e.Cause.Error()
	if cause != e.Cause && cause.Error() != text {
		text += " (" + cause.Error() + ")"
	}
	return fmt.Sprintf("the transfer of %s stopped at %s of %s: %s, retry to resume it", e.Name, roomBytes(e.Held), roomBytes(e.Size), text)
}

func (e *PeerFileTransferError) Unwrap() error { return e.Cause }

// No run exists yet when a file transfer stops, so the attempt is provably not admitted
func (e *PeerFileTransferError) attemptProof() (bool, bool) { return true, false }

// File error of a failed request, a peer refusal, a scoped grant failure, or a stopped transfer
func peerFileError(err error, name string, held, size int64) error {
	var failure *PeerRunsHTTPError
	if errors.As(err, &failure) && failure.StatusCode >= 400 && failure.StatusCode < 500 {
		if peerFileRefusalCodes[failure.ErrorCode] {
			detail := failure.Detail
			if detail == "" {
				detail = failure.Msg
			}
			return &PeerFileRefusedError{Name: name, Size: size, Detail: detail}
		}
		if failure.StatusCode == 401 || failure.StatusCode == 403 {
			return err
		}
	}
	return &PeerFileTransferError{Name: name, Held: held, Size: size, Cause: err}
}

// Sends one turn file in chunks from the offset the peer holds, the peer checking the whole digest
func (c *PeerRunsHTTPClient) SendFile(_ context.Context, grant string, file map[string]any, data []byte) (map[string]any, error) {
	scoped, err := peerRequireRoomGrant(grant)
	if err != nil {
		return nil, err
	}
	name, size := pyrepr.Str(file["name"]), int64(len(data))
	begin := pyjson.ObjectOf("name", file["name"], "kind", file["kind"], "mime", file["mime"], "size", size, "sha256", file["sha256"])
	var held int64
	for range 4 {
		answer, err := c.request("/v1/room-members/files", "POST", begin, nil, scoped)
		if err != nil {
			return nil, peerFileError(err, name, held, size)
		}
		fileID := hostedStrOrEmpty(answer["file_id"])
		held, _ = hostedStrictInt(answer["received"])
		if fileID == "" || held < 0 || held > size {
			return nil, newPeerRunsHTTPError("peer answered an invalid file offset", peerErrorFlags{}, nil)
		}
		if held > 0 && held < size {
			pkgLog().Info(fmt.Sprintf("hosted room file %s resumed on the peer at %d of %d bytes", name, held, size))
		}
		lost := false
		for held < size && !lost {
			end := min(size, held+peerFileChunkBytes)
			answer, err = c.request("/v1/room-members/files/"+pyurl.Quote(fileID, ""), "PUT",
				pyjson.ObjectOf("offset", held, "data_base64", base64.StdEncoding.EncodeToString(data[held:end])), nil, scoped)
			var failure *PeerRunsHTTPError
			switch {
			case errors.As(err, &failure) && failure.ErrorCode == "attachment_offset":
				lost = true
			case err != nil:
				return nil, peerFileError(err, name, held, size)
			default:
				if held, _ = hostedStrictInt(answer["received"]); held < end {
					return nil, newPeerRunsHTTPError("peer did not take the file chunk", peerErrorFlags{}, nil)
				}
			}
		}
		if !lost {
			if !pyrepr.Truthy(answer["complete"]) {
				return nil, newPeerRunsHTTPError("peer did not complete the file", peerErrorFlags{}, nil)
			}
			return answer, nil
		}
	}
	return nil, &PeerFileTransferError{Name: name, Held: held, Size: size, Cause: errors.New("the peer kept losing its place in the file")}
}

// Reply files of a finished peer run pulled into the room store, digests checked, refusals kept as notes
func (c *PeerRunsHTTPClient) replyFiles(record map[string]any, listed any, grant string) ([]any, []string, error) {
	list, ok := pyjson.Plain(listed).([]any)
	if !ok {
		return nil, nil, newPeerRunsHTTPError("peer listed its reply files in an invalid shape", peerErrorFlags{}, nil)
	}
	if len(list) == 0 {
		return nil, nil, nil
	}
	scoped, err := peerRequireRoomGrant(grant)
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	dbPath := c.ReceiptDBPath
	c.mu.Unlock()
	if dbPath == "" {
		return nil, nil, newPeerRunsHTTPError("peer reply files need the room store of this gateway", peerErrorFlags{}, nil)
	}
	store, err := NewHostedRoomAttachmentStore(c.ctx, dbPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, nil, err
	}
	roomID, runID := pyrepr.Str(record["room_id"]), pyrepr.Str(record["run_id"])
	manifest, notes := []any{}, []string{}
	var total int64
	for index, raw := range list {
		item, _ := raw.(map[string]any)
		name := roomFileName(pyrepr.Str(item["name"]))
		fileID := hostedStrOrEmpty(item["file_id"])
		size, sized := hostedStrictInt(item["size"])
		digest, err := hostedDigest(item["sha256"], "reply file sha256")
		if fileID == "" || !sized || err != nil {
			return nil, nil, newPeerRunsHTTPError("peer listed an invalid reply file", peerErrorFlags{}, err)
		}
		switch {
		case len(manifest) >= maxAttachmentsPerMessage:
			notes = append(notes, fmt.Sprintf("[%s was not shared: one message brings at most %d files]", name, maxAttachmentsPerMessage))
			continue
		case size <= 0 || size > maxAttachmentBytes:
			notes = append(notes, fmt.Sprintf("[%s was not shared: %s is over the %s limit for one room file]", name, roomBytes(size), roomBytes(maxAttachmentBytes)))
			continue
		case total+size > maxMessageAttachmentBytes:
			notes = append(notes, fmt.Sprintf("[%s was not shared: the files of one message stay under %s]", name, roomBytes(maxMessageAttachmentBytes)))
			continue
		}
		uploadID := "peer." + runID + "." + strconv.Itoa(index)
		existing, err := store.FindUpload(roomID, uploadID)
		if err != nil {
			return nil, nil, err
		}
		if existing == nil {
			data, err := c.pullFile(fileID, name, size, digest, scoped)
			if err != nil {
				return nil, nil, err
			}
			stored, err := storeRoomFile(store, roomID, uploadID, name, data)
			var refused *AttachmentError
			if errors.As(err, &refused) {
				notes = append(notes, fmt.Sprintf("[%s was not shared: %s]", name, refused.Msg))
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			existing = stored
		} else if existing["sha256"] != digest {
			return nil, nil, newPeerRunsHTTPError("peer reply file changed after it was stored", peerErrorFlags{}, nil)
		}
		manifest = append(manifest, map[string]any{"attachment_id": existing["attachment_id"], "kind": existing["kind"],
			"name": existing["name"], "mime": existing["mime"], "size": existing["size"]})
		total += size
	}
	return manifest, notes, nil
}

// Bytes of one peer reply file, pulled in chunks from what this client already holds and checked against the digest
func (c *PeerRunsHTTPClient) pullFile(fileID, name string, size int64, digest, grant string) ([]byte, error) {
	c.mu.Lock()
	data := append([]byte(nil), c.partials[fileID]...)
	c.mu.Unlock()
	if len(data) > 0 && int64(len(data)) < size {
		pkgLog().Info(fmt.Sprintf("hosted room reply file %s resumed at %d of %d bytes", name, len(data), size))
	}
	for int64(len(data)) < size {
		path := "/v1/room-members/files/" + pyurl.Quote(fileID, "") + "?offset=" + strconv.Itoa(len(data))
		answer, err := c.request(path, "GET", nil, nil, grant)
		if err != nil {
			c.mu.Lock()
			c.partials[fileID] = data
			c.mu.Unlock()
			return nil, newPeerRunsHTTPError(fmt.Sprintf("the reply file %s stopped at %s of %s", name, roomBytes(int64(len(data))), roomBytes(size)),
				peerErrorFlags{retryable: true}, err)
		}
		chunk, err := pybase64.B64DecodeValidate(pyrepr.Str(answer["data_base64"]))
		if err != nil || len(chunk) == 0 || int64(len(data)+len(chunk)) > size {
			return nil, newPeerRunsHTTPError("peer sent an invalid reply file chunk", peerErrorFlags{}, err)
		}
		data = append(data, chunk...)
	}
	c.mu.Lock()
	delete(c.partials, fileID)
	c.mu.Unlock()
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, newPeerRunsHTTPError(fmt.Sprintf("peer reply file %s failed its SHA-256 check", name), peerErrorFlags{retryable: true}, nil)
	}
	return data, nil
}
