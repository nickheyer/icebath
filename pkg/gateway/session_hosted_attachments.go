package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
)

// File refusals no retry can change, the member's media cap, binding, expiry or corruption
var hostedPermanentFileRefusals = map[string]bool{"invalid_params": true, "permission_denied": true}

// Commits the message's files to every member, appends the user event, and returns a failed commit to upload state
func AppendUserEvent(ctx context.Context, service *HostedRoomService, roomID, eventID string, payload map[string]any, gatewayID string, epoch int64) (map[string]any, error) {
	store, err := NewHostedRoomAttachmentStore(ctx, service.DBPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	manifest, present := payload["attachments"]
	if !present {
		manifest = []any{}
	}
	var transitioned []string
	if pyrepr.Truthy(manifest) {
		room, err := service.room(roomID)
		if err != nil {
			return nil, err
		}
		recipients := []string{}
		members, _ := room["members"].([]any)
		for _, raw := range members {
			member, _ := hostedMapping(raw)
			recipients = append(recipients, pyrepr.Str(member["member_id"]))
		}
		if _, transitioned, err = store.CommitMessageWithReceipt(AttachmentCommit{RoomID: roomID, EventID: eventID, Manifest: manifest,
			RecipientMemberIDs: recipients, ViewerAccess: true, HoldUntilEvent: true}); err != nil {
			return nil, err
		}
	}
	event, err := AppendEvent(ctx, service.DBPath, AppendEventInput{RoomID: roomID, EventID: eventID, Kind: "message.user",
		Actor: map[string]any{"kind": "user", "id": "desktop"}, Payload: payload, AuthorityGatewayID: gatewayID, AuthorityEpoch: epoch})
	if err != nil {
		if len(transitioned) > 0 {
			if _, abortErr := store.AbortMessageCommit(roomID, eventID, transitioned); abortErr != nil {
				return nil, abortErr
			}
		}
		return nil, err
	}
	return event, nil
}

// Room an actor of this profile may use with the capability, after ownership, authority and existence checks
func hostedAuthorize(ctx context.Context, service *CanonicalHostedRoomService, actor Principal, params map[string]any, capability string) (string, error) {
	if actor.ProfileID != service.Authority.ProfileID {
		return "", runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities[capability] {
		return "", runtimeRefusal("permission_denied")
	}
	if _, err := service.AuthorizeRoom(ctx, actor.Subject, params["room_id"], false, nil); err != nil {
		return "", err
	}
	roomID := params["room_id"].(string)
	if _, _, err := service.ownedAuthority(roomID); err != nil {
		return "", err
	}
	if _, err := service.room(roomID); err != nil {
		return "", err
	}
	return roomID, nil
}

// Retains a composer file for an actor allowed to submit to the room
func Upload(ctx context.Context, service *CanonicalHostedRoomService, actor Principal, params map[string]any) (map[string]any, error) {
	roomID, err := hostedAuthorize(ctx, service, actor, params, "session:submit")
	if err != nil {
		return nil, err
	}
	return StoreUpload(ctx, service.HostedRoomService, roomID, params)
}

// Reads a committed room file for an actor allowed to read the room
func Download(ctx context.Context, service *CanonicalHostedRoomService, actor Principal, params map[string]any) (map[string]any, error) {
	roomID, err := hostedAuthorize(ctx, service, actor, params, "session:read")
	if err != nil {
		return nil, err
	}
	return ReadDownload(ctx, service.HostedRoomService, roomID, params)
}

// Retains one composer file for a later groups.send manifest on either route
func StoreUpload(ctx context.Context, service *HostedRoomService, roomID string, params map[string]any) (map[string]any, error) {
	encoded, ok := params["data_base64"].(string)
	if !ok || utf8.RuneCountInString(encoded) > ((maxAttachmentBytes+2)/3)*4 {
		return nil, runtimeRefusal("invalid_params")
	}
	data, err := pybase64.B64DecodeValidate(encoded)
	if err != nil {
		return nil, &state.RuntimeStoreError{Reason: "invalid_params", Cause: err}
	}
	store, err := NewHostedRoomAttachmentStore(ctx, service.DBPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	return store.Put(roomID, params["upload_id"], params["kind"], params["name"], params["mime"], data)
}

// One committed file with its bytes as base64 for a live room viewer on either route
func ReadDownload(ctx context.Context, service *HostedRoomService, roomID string, params map[string]any) (map[string]any, error) {
	if !pyrepr.Truthy(params["event_id"]) {
		return nil, runtimeRefusal("invalid_params")
	}
	gatewayID, epoch, err := service.ownedAuthority(roomID)
	if err != nil {
		return nil, err
	}
	store, err := NewHostedRoomAttachmentStore(ctx, service.DBPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	saved, err := store.ReadViewer(roomID, params["attachment_id"], params["event_id"], nil, gatewayID, epoch)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(saved.Attachment)+1)
	maps.Copy(out, saved.Attachment)
	out["data_base64"] = base64.StdEncoding.EncodeToString(saved.Data)
	return out, nil
}

// Turn payload of the producer's member from event bound authorized bytes only, never a caller path
func SubmissionPayload(ctx context.Context, rpc *HostedRoomAuthorityRPC, prompt string, attachments any) (map[string]any, error) {
	return BoundSubmissionPayload(ctx, rpc.Authority.DB.Path(), rpc.RoomID, rpc.MemberID, prompt, attachments, rpc.HostedAttachmentData)
}

// Committed admission payload of the producer's turn, its staged images admitted as durable media fields
func CommittedSubmissionPayload(ctx context.Context, rpc *HostedRoomAuthorityRPC, prompt string, attachments any) (*pyjson.Object, error) {
	payload, err := SubmissionPayload(ctx, rpc, prompt, attachments)
	if err != nil {
		return nil, err
	}
	admitted, err := admitAttachments(ctx, payload["attachments"], nil)
	if err != nil {
		return nil, err
	}
	out := pyjson.ObjectOf("text", payload["text"])
	for _, key := range admitted.Keys {
		out.Set(key, admitted.Vals[key])
	}
	return out, nil
}

// One validated bound manifest item and the bytes the authority transferred for it
type TransferredAttachment struct {
	Item map[string]any
	Data []byte
}

// Member turn text with document path lines and owner local image staging paths, identical on both routes
func BoundSubmissionPayload(ctx context.Context, dbPath, roomID string, memberID any, prompt string, attachments any, transferred *[]TransferredAttachment) (map[string]any, error) {
	if !pyrepr.Truthy(attachments) {
		return map[string]any{"text": prompt}, nil
	}
	manifest, err := validateBoundTaskManifest(attachments)
	if err != nil {
		return nil, err
	}
	sizes := make([]int64, len(manifest))
	for i, item := range manifest {
		sizes[i] = int64(item["size"].(int))
	}
	if err := validateMediaBatchSize(ctx, sizes); err != nil {
		return nil, err
	}
	store, err := NewHostedRoomAttachmentStore(ctx, dbPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	if transferred != nil && !transferredMatches(*transferred, manifest) {
		return nil, runtimeRefusal("permission_denied")
	}
	references := []any{}
	for index, item := range manifest {
		var data []byte
		if transferred == nil {
			saved, err := store.Read(roomID, item["attachment_id"], memberID, item["event_id"], false)
			if err != nil {
				return nil, err
			}
			for _, key := range []string{"kind", "name", "mime", "size"} {
				if !pyrepr.Equal(saved.Attachment[key], item[key]) {
					return nil, runtimeRefusal("permission_denied")
				}
			}
			data = saved.Data
		} else {
			data = (*transferred)[index].Data
		}
		if int64(len(data)) != sizes[index] {
			return nil, runtimeRefusal("permission_denied")
		}
		reference, err := captureRoomInput(ctx, item["name"].(string), data)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	paths, err := restoreNativeMedia(ctx, references)
	if err != nil {
		return nil, err
	}
	images := []any{}
	var documents []string
	for i, path := range paths {
		item := manifest[i]
		mime := item["mime"].(string)
		if !attachmentMimes[mime] {
			documents = append(documents, path)
			continue
		}
		target, err := stageRoomImage(ctx, path, item["name"].(string))
		if err != nil {
			return nil, err
		}
		images = append(images, map[string]any{"path": target, "mime": mime})
	}
	text := prompt
	for _, path := range documents {
		text += HostedDocumentLine + path + "\n"
	}
	out := map[string]any{"text": text}
	if len(images) > 0 {
		out["attachments"] = images
	}
	return out, nil
}

// Whether the transferred items are exactly the validated manifest
func transferredMatches(transferred []TransferredAttachment, manifest []map[string]any) bool {
	if len(transferred) != len(manifest) {
		return false
	}
	for i, pair := range transferred {
		if !pyrepr.Equal(pair.Item, manifest[i]) {
			return false
		}
	}
	return true
}

// Content addressed retained reference of one bound file written under its manifest name in a private directory
func captureRoomInput(ctx context.Context, name string, data []byte) (any, error) {
	directory, err := os.MkdirTemp("", "icebath-room-input-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, name)
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := output.Write(data); err != nil {
		output.Close()
		return nil, err
	}
	if err := output.Close(); err != nil {
		return nil, err
	}
	references, err := captureNativeMedia(ctx, []string{path})
	if err != nil {
		return nil, err
	}
	return references[0], nil
}

// Owner local image staging path of retained room bytes under a deterministic name for exact retries
func stageRoomImage(ctx context.Context, path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	cache, err := mediacache.ImageCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return "", err
	}
	staging, err := pypath.Resolve(cache)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(staging, 0o777); err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	target := filepath.Join(staging, hex.EncodeToString(digest[:])+pathSuffix(name))
	found, err := pypath.PathlibStat(target)
	if err != nil {
		return "", err
	}
	if found != nil {
		info, err := os.Lstat(target)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", runtimeRefusal("storage_unavailable")
		}
		existing, err := os.ReadFile(target)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(existing, data) {
			return "", runtimeRefusal("storage_unavailable")
		}
		return target, nil
	}
	output, err := os.CreateTemp(staging, "tmp")
	if err != nil {
		return "", err
	}
	temporary := output.Name()
	defer os.Remove(temporary)
	if _, err := output.Write(data); err != nil {
		output.Close()
		return "", err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return "", err
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	return target, os.Rename(temporary, target)
}

// Runs a member turn's file preparation, a failure marked refused before submit or proven not admitted
func PrepareBeforeSubmit[T any](prepare func() (T, error)) (T, error) {
	result, err := prepare()
	if err == nil {
		return result, nil
	}
	var refusal *state.RuntimeStoreError
	permanent := errAs[*AttachmentError](err) || errAs[*DriverValidationError](err) ||
		(errors.As(err, &refusal) && hostedPermanentFileRefusals[refusal.Reason])
	var zero T
	return zero, &HostedAttemptError{Err: err, RefusedBeforeSubmit: permanent, NotAdmitted: !permanent}
}
