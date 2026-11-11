package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Raw bytes per private socket exchange, 480 KiB as base64 under the 512 KiB response line
const hostedChunkBytes = 360 * 1024

// Operations a target owner produces for an attested source binding
var hostedOperations = map[string]bool{"resolve_exact": true, "create": true, "resume": true, "submit": true, "history": true,
	"results": true, "info": true, "interrupt": true, "discard": true, "approve": true}

// Source owner attestation of one hosted operation, naming the durable room owner and the target home
type HostedAttester func(ctx context.Context, selector map[string]any, operation string, params map[string]any) (map[string]any, error)

// Service that attests operations of rooms its authority owns
type hostedAttestingService interface {
	Attest(ctx context.Context, selector map[string]any, operation string, params map[string]any) (map[string]any, error)
}

// Whether the store's metadata of a bound input matches its manifest item
func hostedItemMatches(saved, item map[string]any) bool {
	for _, key := range []string{"kind", "name", "mime", "size"} {
		if !pyrepr.Equal(saved[key], item[key]) {
			return false
		}
	}
	return true
}

// One base64 slice of a bound source input with its upload verified digest, read on the source owner
func sourceAttachmentChunk(ctx context.Context, dbPath, member, roomID string, manifest []map[string]any, params map[string]any) (map[string]any, error) {
	index, indexOK := hostedStrictInt(params["index"])
	offset, offsetOK := hostedStrictInt(params["offset"])
	if !indexOK || index < 0 || index >= int64(len(manifest)) {
		return nil, runtimeRefusal("permission_denied")
	}
	size, _ := hostedStrictInt(manifest[index]["size"])
	if !offsetOK || offset < 0 || offset >= size {
		return nil, runtimeRefusal("permission_denied")
	}
	item := manifest[index]
	store, err := NewHostedRoomAttachmentStore(ctx, dbPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	saved, err := store.ReadRange(roomID, item["attachment_id"], member, item["event_id"], offset, hostedChunkBytes)
	if err != nil {
		return nil, err
	}
	if !hostedItemMatches(saved.Attachment, item) {
		return nil, runtimeRefusal("permission_denied")
	}
	return map[string]any{"data_base64": base64.StdEncoding.EncodeToString(saved.Data), "sha256": saved.Attachment["sha256"]}, nil
}

// Upload verified digest of every bound input, read on the source owner without bytes
func sourceAttachmentDigests(ctx context.Context, dbPath, member, roomID string, manifest []map[string]any) ([]any, error) {
	store, err := NewHostedRoomAttachmentStore(ctx, dbPath, AttachmentStoreOptions{})
	if err != nil {
		return nil, err
	}
	digests := []any{}
	for _, item := range manifest {
		saved, err := store.Describe(roomID, item["attachment_id"], member, item["event_id"])
		if err != nil {
			return nil, err
		}
		if !hostedItemMatches(saved, item) {
			return nil, runtimeRefusal("permission_denied")
		}
		digests = append(digests, saved["sha256"])
	}
	return digests, nil
}

// Bytes of every attested input pulled chunk by chunk from the source owner, each chunk task fenced
func hostedAttachmentData(ctx context.Context, binding *pyjson.Object, attested *pyjson.Object, params map[string]any) ([]TransferredAttachment, error) {
	rawManifest, has := attested.Get("attachments")
	if !has {
		rawManifest = []any{}
	}
	if !pyrepr.Truthy(rawManifest) {
		return []TransferredAttachment{}, nil
	}
	manifest, err := validateBoundTaskManifest(rawManifest)
	if err != nil {
		return nil, err
	}
	rawDigests, _ := attested.Get("attachment_digests")
	digests, ok := pyjson.Plain(rawDigests).([]any)
	if !ok || len(digests) != len(manifest) {
		return nil, runtimeRefusal("permission_denied")
	}
	prompt, _ := attested.Get("prompt")
	promptText, _ := prompt.(string)
	promptDigest := sha256.Sum256([]byte(promptText))
	owner, _ := attested.Get("owner")
	manifestList := make([]any, len(manifest))
	for i, item := range manifest {
		manifestList[i] = item
	}
	result := []TransferredAttachment{}
	for index, item := range manifest {
		size := int64(item["size"].(int))
		var data []byte
		for int64(len(data)) < size {
			chunk, err := hostedAttest(ctx, binding, "attachment", map[string]any{
				"task": params["task"], "execution_generation": params["execution_generation"],
				"prompt_sha256": hex.EncodeToString(promptDigest[:]), "attachments": manifestList, "index": index, "offset": len(data)})
			if err != nil {
				return nil, err
			}
			encoded, err := pyItem(chunk, "data_base64")
			if err != nil {
				return nil, err
			}
			text, _ := encoded.(string)
			raw, err := pybase64.B64DecodeValidate(text)
			if err != nil {
				return nil, err
			}
			expected := min(int64(hostedChunkBytes), size-int64(len(data)))
			chunkOwner, err := pyItem(chunk, "owner")
			if err != nil {
				return nil, err
			}
			chunkDigest, err := pyItem(chunk, "sha256")
			if err != nil {
				return nil, err
			}
			if !pyrepr.Equal(chunkOwner, owner) || int64(len(raw)) != expected || !pyrepr.Equal(chunkDigest, digests[index]) {
				return nil, runtimeRefusal("permission_denied")
			}
			data = append(data, raw...)
		}
		digest := sha256.Sum256(data)
		if !pyrepr.Equal(hex.EncodeToString(digest[:]), digests[index]) {
			return nil, runtimeRefusal("permission_denied")
		}
		result = append(result, TransferredAttachment{Item: item, Data: data})
	}
	return result, nil
}

// Exact selector of a producer envelope, refused unless it names room, member and profile only
func hostedSelector(value any) (map[string]any, bool) {
	selector, ok := hostedMapping(pyjson.Plain(value))
	if !ok || len(selector) != 3 {
		return nil, false
	}
	for _, key := range []string{"room_id", "member_id", "profile"} {
		if _, has := selector[key]; !has {
			return nil, false
		}
	}
	return selector, true
}

// Installs profile selected private routing of hosted attest and producer calls over the authority registry
func InstallHostedTransport(server *GatewayControlServer, authority *SessionAuthority, loop context.Context, attest HostedAttester) {
	selectAuthority := func(envelope map[string]any) (*SessionAuthority, map[string]any, error) {
		params := make(map[string]any, len(envelope))
		for key, value := range envelope {
			if key != "profile_id" {
				params[key] = value
			}
		}
		profileID, ok := envelope["profile_id"].(string)
		if !ok || !filepath.IsAbs(pypath.PurePathStr(profileID)) {
			return nil, nil, runtimeRefusal("profile_mismatch")
		}
		profileHome := pypath.PurePathStr(profileID)
		resolved, err := pypath.Resolve(profileHome)
		if err != nil {
			return nil, nil, err
		}
		if pypath.Normcase(pypath.PurePathStr(resolved)) != pypath.Normcase(profileHome) {
			return nil, nil, runtimeRefusal("profile_mismatch")
		}
		selected := authority
		var runner any
		if authority.Runner != nil {
			runner = authority.Runner
		}
		if registry := registryOf(runner); registry != nil {
			selected = registry.ForHome(profileHome)
		}
		if selected == nil || selected.ProfileID != profileID {
			return nil, nil, runtimeRefusal("profile_mismatch")
		}
		if err := home.AssertProfileLive(profileHome); err != nil {
			return nil, nil, err
		}
		return selected, params, nil
	}
	produce := func(ctx context.Context, selected *SessionAuthority, envelope map[string]any) (any, error) {
		if len(envelope) != 4 {
			return nil, runtimeRefusal("invalid_params")
		}
		for _, key := range []string{"source_home", "selector", "operation", "params"} {
			if _, has := envelope[key]; !has {
				return nil, runtimeRefusal("invalid_params")
			}
		}
		operation, _ := envelope["operation"].(string)
		wire, isMap := hostedMapping(pyjson.Plain(envelope["params"]))
		if !isMap {
			return nil, runtimeRefusal("invalid_params")
		}
		params := make(map[string]any, len(wire))
		for key, value := range wire {
			params[key] = value
		}
		selector, ok := hostedSelector(envelope["selector"])
		if !hostedOperations[operation] || !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		if !pyrepr.Equal(selector["profile"], servedProfileName(pypath.PurePathStr(selected.ProfileID))) {
			return nil, runtimeRefusal("profile_mismatch")
		}
		binding := pyjson.ObjectOf("source_home", envelope["source_home"], "selector", selector, "target_home", selected.ProfileID)
		attested, err := hostedAttest(ctx, binding, operation, params)
		if err != nil {
			return nil, err
		}
		owner, err := pyItem(attested, "owner")
		if err != nil {
			return nil, err
		}
		binding.Set("owner", owner)
		principal, err := hostedPrincipal(selected, binding)
		if err != nil {
			return nil, err
		}
		roomID, _ := selector["room_id"].(string)
		memberID, _ := selector["member_id"].(string)
		profile, _ := selector["profile"].(string)
		rpc, err := NewHostedRoomAuthorityRPC(selected, loop, roomID, memberID, profile, principal,
			func(context.Context, string, *TaskIdentity, *int64) (bool, error) { return true, nil }, nil)
		if err != nil {
			return nil, err
		}
		key := hostedTransportBinding + rpc.Ref.SessionID
		encoded, err := pyjson.Dumps(binding, pyjson.SortKeys(true))
		if err != nil {
			return nil, err
		}
		if err := selected.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			if err := state.RuntimeEpoch(ctx, conn, selected.Epoch); err != nil {
				return err
			}
			old, err := state.ConnRow(ctx, conn, "SELECT value FROM state_meta WHERE key=?", key)
			if err != nil {
				return err
			}
			if old != nil && !pyrepr.Equal(old["value"], encoded) {
				return runtimeRefusal("permission_denied")
			}
			_, err = conn.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta(key,value) VALUES(?,?)", key, encoded)
			return err
		}); err != nil {
			return nil, err
		}
		if operation == "submit" {
			transferred, err := hostedAttachmentData(ctx, binding, attested, params)
			if err != nil {
				return nil, err
			}
			rpc.HostedAttachmentData = &transferred
			delete(params, "prompt_sha256")
			prompt, err := pyItem(attested, "prompt")
			if err != nil {
				return nil, err
			}
			attachments, err := pyItem(attested, "attachments")
			if err != nil {
				return nil, err
			}
			params["prompt"] = prompt
			if pyrepr.Truthy(attachments) {
				params["attachments"] = pyjson.Plain(attachments)
			} else {
				params["attachments"] = nil
			}
			task, err := taskIdentityFromKwargs(pyjson.Plain(params["task"]))
			if err != nil {
				return nil, err
			}
			params["task"] = task
			params["on_terminal"] = func(map[string]any) error { return nil }
		}
		return rpc.call(ctx, operation, params)
	}
	source := func(envelope map[string]any, peer string) (any, error) {
		selected, params, err := selectAuthority(envelope)
		if err != nil {
			return nil, err
		}
		if len(params) != 3 {
			return nil, runtimeRefusal("invalid_params")
		}
		for _, key := range []string{"selector", "operation", "params"} {
			if _, has := params[key]; !has {
				return nil, runtimeRefusal("invalid_params")
			}
		}
		operation, _ := params["operation"].(string)
		if !hostedOperations[operation] && operation != "execute" && operation != "attachment" {
			return nil, runtimeRefusal("invalid_params")
		}
		callback := attest
		if selected != authority {
			callback = nil
			if service, ok := selected.HostedRoomService.(hostedAttestingService); ok {
				callback = service.Attest
			}
		}
		if callback == nil {
			return nil, runtimeRefusal("runtime_draining")
		}
		selector, ok := hostedMapping(pyjson.Plain(params["selector"]))
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		wire, ok := hostedMapping(pyjson.Plain(params["params"]))
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		return callback(ownerScope(loop, selected, false), selector, operation, wire)
	}
	target := func(envelope map[string]any, peer string) (any, error) {
		selected, params, err := selectAuthority(envelope)
		if err != nil {
			return nil, err
		}
		return produce(ownerScope(loop, selected, false), selected, params)
	}
	server.AddPrivateHandlers(map[string]ControlPrivateHandler{"hosted-attest": source, "hosted-producer": target})
}

// Driver compatible producer that forwards every operation to the member's owner process, carrying data only
type HostedRoomOwnerRPC struct {
	HostedRoomAuthorityRPC
	Home      string
	binding   map[string]any
	pending   *pydict.Dict[string, func(map[string]any) error]
	lock      sync.Mutex
	monitored bool
	hasRef    bool
}

// Producer forwarding to the owner at the home for the source room member and profile
func NewHostedRoomOwnerRPC(ownerHome, sourceHome, roomID, memberID, profile string) *HostedRoomOwnerRPC {
	o := &HostedRoomOwnerRPC{Home: pypath.PurePathStr(ownerHome), pending: pydict.New[string, func(map[string]any) error]()}
	o.MemberID, o.RoomID, o.Profile = memberID, roomID, profile
	o.binding = map[string]any{"source_home": pypath.PurePathStr(sourceHome),
		"selector": map[string]any{"room_id": roomID, "member_id": memberID, "profile": profile}}
	o.self = o
	return o
}

// Session the owner named for the binding, false before a create, resume or exact resolution
func (o *HostedRoomOwnerRPC) currentRef() (SessionRef, bool) {
	o.lock.Lock()
	defer o.lock.Unlock()
	return o.Ref, o.hasRef
}

// Sends the operation to the owner, hashing a submitted prompt and watching receipts for its callback
func (o *HostedRoomOwnerRPC) call(ctx context.Context, operation string, params map[string]any) (any, error) {
	sent := make(map[string]any, len(params))
	for key, value := range params {
		if key != "on_terminal" {
			sent[key] = value
		}
	}
	callback, _ := params["on_terminal"].(func(map[string]any) error)
	if operation == "submit" {
		prompt, ok := sent["prompt"].(string)
		if !ok {
			return nil, errors.New("'" + pyrepr.TypeName(sent["prompt"]) + "' object has no attribute 'encode'")
		}
		delete(sent, "prompt")
		digest := sha256.Sum256([]byte(prompt))
		sent["prompt_sha256"] = hex.EncodeToString(digest[:])
	}
	if task, ok := sent["task"].(TaskIdentity); ok {
		sent["task"] = task.AsDict()
	}
	request := map[string]any{"source_home": o.binding["source_home"], "selector": o.binding["selector"], "operation": operation, "params": sent}
	raw, err := ownerRequest(ctx, o.Home, "hosted-producer", request, 30*time.Second)
	if err != nil {
		return nil, err
	}
	result := pyjson.Plain(raw)
	if (operation == "create" || operation == "resume" || operation == "resolve_exact") && result != nil {
		mapping, _ := hostedMapping(result)
		sessionID, has := mapping["session_id"]
		if !has {
			return nil, &pyKeyError{"session_id"}
		}
		o.lock.Lock()
		o.Ref, o.hasRef = SessionRef{ProfileID: o.Home, SessionID: pyrepr.Str(sessionID)}, true
		o.lock.Unlock()
	}
	if operation == "submit" && callback != nil {
		mapping, _ := hostedMapping(result)
		admissionID, has := mapping["admission_id"]
		if !has {
			return nil, &pyKeyError{"admission_id"}
		}
		sessionID := pyrepr.Str(params["session_id"])
		o.lock.Lock()
		o.pending.Set(pyrepr.Str(admissionID), callback)
		if !o.monitored {
			o.monitored = true
			go o.watch(sessionID)
		}
		o.lock.Unlock()
	}
	if operation == "history" || operation == "results" {
		if err := o.deliver(result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Recovery history, falling back to the bounded receipt rows when the transcript passes one response line
func (o *HostedRoomOwnerRPC) history(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error) {
	rows, err := o.HostedRoomAuthorityRPC.history(ctx, profile, sessionID, source)
	var refusal *state.RuntimeStoreError
	if err == nil || !errors.As(err, &refusal) || refusal.Reason != "response_too_large" {
		return rows, err
	}
	receipts, err := o.Results(ctx, profile, sessionID, source, nil)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(receipts))
	for i, receipt := range receipts {
		row := make(map[string]any, len(receipt)+2)
		for key, value := range receipt {
			row[key] = value
		}
		row["role"], row["content"] = "assistant", receipt["text"]
		out[i] = row
	}
	return out, nil
}

// Hands each receipt to the callback still waiting on its settlement id
func (o *HostedRoomOwnerRPC) deliver(history any) error {
	rows, _ := history.([]any)
	for _, raw := range rows {
		row, _ := hostedMapping(raw)
		o.lock.Lock()
		key := pyrepr.Str(row["settlement_id"])
		callback, found := o.pending.Get(key)
		if found {
			o.pending.Delete(key)
		}
		o.lock.Unlock()
		if found && callback != nil {
			if err := callback(row); err != nil {
				return err
			}
		}
	}
	return nil
}

// Polls per admission receipts until no callback waits, leaving callbacks for driver recovery on a failure
func (o *HostedRoomOwnerRPC) watch(sessionID string) {
	for {
		o.lock.Lock()
		if o.pending.Len() == 0 {
			o.monitored = false
			o.lock.Unlock()
			return
		}
		keys := o.pending.Keys()
		if len(keys) > 256 {
			keys = keys[:256]
		}
		o.lock.Unlock()
		wanted := make([]any, len(keys))
		for i, key := range keys {
			wanted[i] = key
		}
		selector := o.binding["selector"].(map[string]any)
		if _, err := o.Results(context.Background(), pyrepr.Str(selector["profile"]), sessionID, RoomSessionSource, wanted); err != nil {
			o.lock.Lock()
			o.monitored = false
			o.lock.Unlock()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}
