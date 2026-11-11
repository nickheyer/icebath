package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Adapter metadata keys of an ordinary WhatsApp inbound that cross native admission
var whatsappMetadata = map[string]bool{"whatsapp_native_type": true, "whatsapp_native": true, "whatsapp_from_owner": true}

// Top level native metadata keys the WhatsApp bridge emits
var whatsappNativeKeys = map[string]bool{"video": true, "audio": true, "sticker": true, "location": true, "contact": true, "contacts": true,
	"reaction": true, "poll": true, "pollUpdate": true}

// Largest encoded native metadata a replay keeps
const whatsappNativeMaxBytes = 16 * 1024

// Message types an event may carry
var messageTypes = map[MessageType]bool{MessageTypeText: true, MessageTypeLocation: true, MessageTypePhoto: true, MessageTypeVideo: true,
	MessageTypeAudio: true, MessageTypeVoice: true, MessageTypeDocument: true, MessageTypeSticker: true, MessageTypeCommand: true}

// Runner surface that authorizes a native sender under the live transport of its source
type nativeAuthorizationRunner interface {
	AdapterFlag(platform Platform, name string, profile *string) bool
	AdapterProfileForSource(source *SessionSource) *string
	IsUserAuthorizedForSource(ctx context.Context, source *SessionSource, allowAdapterDelegation bool) (bool, error)
}

// Keys of a mapping value, false for any other value
func mappingKeys(value any) ([]string, bool) {
	switch v := value.(type) {
	case *pyjson.Object:
		return v.Keys, true
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		return keys, true
	}
	return nil, false
}

// JSON projection of the adapter metadata a native replay keeps, false when the event must be refused
func nativeMetadata(event *MessageEvent) (*pyjson.Object, bool) {
	metadata := event.Metadata
	if len(metadata) == 0 {
		return pyjson.NewObject(), true
	}
	if event.Source == nil || event.Source.Platform != PlatformWhatsApp {
		return nil, false
	}
	for key := range metadata {
		if !whatsappMetadata[key] {
			return nil, false
		}
	}
	kind, native := metadata["whatsapp_native_type"], metadata["whatsapp_native"]
	if kind != nil {
		name, isName := kind.(string)
		if !isName || pystr.Len(name) == 0 || pystr.Len(name) > 64 {
			return nil, false
		}
	}
	if owner := metadata["whatsapp_from_owner"]; owner != nil && !pyjson.Equal(owner, true) {
		return nil, false
	}
	if native != nil {
		keys, isMapping := mappingKeys(native)
		if !isMapping {
			return nil, false
		}
		for _, key := range keys {
			if !whatsappNativeKeys[key] {
				return nil, false
			}
		}
	}
	projected := pyjson.NewObject()
	for _, key := range []string{"whatsapp_native_type", "whatsapp_from_owner"} {
		if value, has := metadata[key]; has {
			projected.Set(key, value)
		}
	}
	if native != nil {
		encoded, err := pyjson.Dumps(native, pyjson.EnsureASCII(false), pyjson.AllowNaN(false))
		if err != nil {
			return nil, false
		}
		if len(encoded) <= whatsappNativeMaxBytes {
			decoded, err := pyjson.Loads(encoded)
			if err != nil {
				return nil, false
			}
			projected.Set("whatsapp_native", decoded)
		}
	}
	return projected, true
}

// Copy of an event holding only its dataclass fields over the same source object
func replaceEventFields(event *MessageEvent) *MessageEvent {
	copied := replaceMessageEvent(event)
	copied.Source, copied.claimedOwner = event.Source, nil
	return copied
}

// Event at the envelope boundary, frozen over a copy of its source when asked, its unreachable remote media dropped
func adapterEvent(event *MessageEvent, freezeSource bool) *MessageEvent {
	if freezeSource {
		event = replaceEventFields(event)
		if event.Source != nil {
			source := *event.Source
			event.Source = &source
		}
	}
	return dropRemoteMedia(event)
}

// Event without the media entries an adapter left as remote URLs, each named to the agent in the text instead
func dropRemoteMedia(event *MessageEvent) *MessageEvent {
	urls, types := event.MediaURLs, event.MediaTypes
	local := true
	for _, url := range urls {
		local = local && pypath.IsAbs(url)
	}
	if mediaClause(event) != "" || local {
		return event
	}
	var kept []int
	lines := []string{}
	if event.Text != "" {
		lines = append(lines, event.Text)
	}
	for index, url := range urls {
		if pypath.IsAbs(url) {
			kept = append(kept, index)
			continue
		}
		kind := "file"
		if len(types) > 0 {
			kind = types[index]
		}
		lines = append(lines, "[The user sent an attachment ("+kind+") that could not be retrieved.]")
	}
	replaced := replaceEventFields(event)
	replaced.Text = strings.Join(lines, "\n")
	replaced.MediaURLs, replaced.MediaTypes = keepIndexes(urls, kept), keepIndexes(types, kept)
	replaced.MediaTextInlined, replaced.MediaFilenames = keepIndexes(event.MediaTextInlined, kept), keepIndexes(event.MediaFilenames, kept)
	return replaced
}

// Values at the kept indexes, an empty list left as it is
func keepIndexes[T any](values []T, kept []int) []T {
	if len(values) == 0 {
		return values
	}
	out := make([]T, 0, len(kept))
	for _, index := range kept {
		out = append(out, values[index])
	}
	return out
}

// Refusal naming the refused clause in the gateway log and on the error, the wire reason kept stable
func refuseNative(event *MessageEvent, reason, clause string) error {
	platform, chat, message := "None", "None", "None"
	if event.Source != nil {
		platform, chat = string(event.Source.Platform), event.Source.ChatID
	}
	if event.MessageID != nil {
		message = *event.MessageID
	}
	slog.Warn(fmt.Sprintf("Native admission refused: %s (%s) platform=%s chat=%s message=%s", reason, clause, platform, chat, message))
	return state.NewRuntimeStoreError(reason, &clause, nil)
}

// Clause of the first media list whose length disagrees with the media urls, empty when they agree
func mediaClause(event *MessageEvent) string {
	count := len(event.MediaURLs)
	if n := len(event.MediaTypes); n != 0 && n != count {
		return "media_types_length"
	}
	if n := len(event.MediaTextInlined); n != 0 && n != count {
		return "media_text_inlined_length"
	}
	if n := len(event.MediaFilenames); n != 0 && n != count {
		return "media_filenames_length"
	}
	return ""
}

// True when a provenance value binds an upstream relay
func provenanceBindsRelay(provenance any) bool {
	switch v := provenance.(type) {
	case *pyjson.Object:
		_, has := v.Get("relay")
		return v != nil && has
	case map[string]any:
		_, has := v["relay"]
		return has
	case string:
		return strings.Contains(v, "relay")
	}
	return false
}

// Clause of the first trust or shape rule a native event breaks, empty when it breaks none
func eventClause(runner AuthorityRunner, event *MessageEvent, provenance any, freshRoles bool) string {
	source := event.Source
	if source == nil {
		return "source"
	}
	if !messageTypes[event.MessageType] {
		return "message_type"
	}
	if command, isCommand := event.GetCommand(); isCommand && command != "" {
		return "command"
	}
	if event.Internal {
		return "internal"
	}
	if _, kept := nativeMetadata(event); !kept {
		return "metadata"
	}
	if len(event.PromptResponse) > 0 {
		return "prompt_response"
	}
	if source.RoleAuthorized && !freshRoles {
		return "role_authorized_without_fresh_roles"
	}
	if source.DeliveredViaUpstreamRelay && !provenanceBindsRelay(provenance) {
		return "upstream_relay_without_provenance"
	}
	if source.ProfileRouteRejected {
		return "profile_route_rejected"
	}
	if source.authorizationProfileHome != "" && provenance == nil {
		return "authorization_home_without_provenance"
	}
	if runner.MultiplexProfiles() && provenance == nil {
		return "multiplex_without_provenance"
	}
	return ""
}

// True for an auto skill that is absent, one name or a list of names
func autoSkillValid(skill any) bool {
	switch v := skill.(type) {
	case nil, string, []string:
		return true
	case []any:
		for _, item := range v {
			if _, isName := item.(string); !isName {
				return false
			}
		}
		return true
	}
	return false
}

// Wire dict of a validated native sender, every trust, authorization and adapter rule checked first
func validateNative(ctx context.Context, runner AuthorityRunner, event *MessageEvent, provenance any, freshRoles bool) (*pyjson.Object, error) {
	source := event.Source
	if clause := eventClause(runner, event, provenance, freshRoles); clause != "" {
		return nil, refuseNative(event, "invalid_params", clause)
	}
	if provenance != nil {
		if _, err := restoreProvenance(ctx, runner, source, provenance); err != nil {
			return nil, err
		}
	}
	authz, ok := runner.(nativeAuthorizationRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(runner) + "' object has no attribute '_is_user_authorized_for_source'"}
	}
	delegated := freshRoles || provenanceBindsRelay(provenance)
	if !delegated {
		delegated = authz.AdapterFlag(source.Platform, "authorization_is_upstream", authz.AdapterProfileForSource(source))
	}
	allowed, err := authz.IsUserAuthorizedForSource(ctx, source, delegated)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, refuseNative(event, "permission_denied", "user_not_authorized")
	}
	if !autoSkillValid(event.AutoSkill) {
		return nil, refuseNative(event, "invalid_params", "auto_skill")
	}
	if clause := mediaClause(event); clause != "" {
		return nil, refuseNative(event, "invalid_params", clause)
	}
	encoded := source.ToDict()
	encoded.Set("is_bot", source.IsBot)
	wire, _ := pyjson.Plain(encoded).(map[string]any)
	restored, err := SessionSourceFromDict(ctx, wire)
	if err != nil {
		return nil, err
	}
	restored.IsBot = source.IsBot
	if provenance != nil {
		if _, err := restoreProvenance(ctx, runner, restored, provenance); err != nil {
			return nil, err
		}
	}
	adapter, err := runner.AdapterForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if adapter == nil {
		return nil, refuseNative(event, "not_found", "adapter")
	}
	restoredAdapter, err := runner.AdapterForSource(ctx, restored)
	if err != nil {
		return nil, err
	}
	if restoredAdapter != adapter {
		return nil, refuseNative(event, "not_found", "adapter_mismatch")
	}
	return encoded, nil
}

// Provenance a capture returned as a plain value, nil when it captured none
func capturedProvenance(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (any, error) {
	captured, err := captureProvenance(ctx, runner, event)
	if err != nil || captured == nil {
		return nil, err
	}
	return captured, nil
}

// Durable payload of a native event validated and captured on the caller's goroutine
func snapshotNativeEvent(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (*pyjson.Object, error) {
	provenance, err := capturedProvenance(ctx, runner, event)
	if err != nil {
		return nil, err
	}
	return snapshotNative(ctx, runner, adapterEvent(event, false), provenance, false, nil, nil)
}

// Durable payload of a native event, its identity frozen before the first yield and its media captured off the caller
func prepareNative(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (*pyjson.Object, error) {
	provenance, err := capturedProvenance(ctx, runner, event)
	if err != nil {
		return nil, err
	}
	frozen := adapterEvent(event, true)
	freshRoles, err := reauthorizeRoles(ctx, runner, frozen.Source, provenance)
	if err != nil {
		return nil, err
	}
	encodedSource, err := validateNative(ctx, runner, frozen, provenance, freshRoles)
	if err != nil {
		return nil, err
	}
	var media []any
	if len(frozen.MediaURLs) > 0 {
		scope := leaseScope(ctx)
		owned := scope != nil && scope.authority != nil
		work := func(context.Context) (any, error) {
			return captureNativeMedia(context.WithoutCancel(ctx), frozen.MediaURLs)
		}
		var capture *trackedTask
		if owned {
			capture = trackMutation(scope.authority, work)
		} else {
			capture = &trackedTask{done: make(chan struct{})}
			go func() {
				defer close(capture.done)
				capture.result, capture.err = work(ctx)
			}()
		}
		select {
		case <-capture.done:
			if capture.err != nil {
				if err := abandonCapture(context.WithoutCancel(ctx), scope); err != nil {
					return nil, err
				}
				return nil, capture.err
			}
			media, _ = capture.result.([]any)
		case <-ctx.Done():
			if owned {
				scope.mu.Lock()
				scope.abandoned = true
				scope.mu.Unlock()
				go func() {
					<-capture.done
					trackMutation(scope.authority, func(taskCtx context.Context) (any, error) {
						return nil, releaseAbandonedCapture(taskCtx, scope)
					})
				}()
			}
			return nil, ctx.Err()
		}
	}
	return snapshotNative(ctx, runner, frozen, provenance, freshRoles, encodedSource, media)
}

// Isoformat of an event time, a local one read as naive and any other zone as aware
func eventISOFormat(at time.Time) string {
	return pyDatetime{T: at, Aware: at.Location() != time.Local}.isoformat()
}

// List of plain values of a typed slice
func plainList[T any](values []T, plain func(T) any) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = plain(value)
	}
	return out
}

// Optional integer as a plain value, nil when absent
func optIntAny(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// Durable payload of a validated native event with its route, sender, context and captured media
func snapshotNative(ctx context.Context, runner AuthorityRunner, event *MessageEvent, provenance any, freshRoles bool, encodedSource *pyjson.Object, media []any) (*pyjson.Object, error) {
	if encodedSource == nil {
		var err error
		if encodedSource, err = validateNative(ctx, runner, event, provenance, freshRoles); err != nil {
			return nil, err
		}
	}
	store := runner.SessionStore()
	route, err := store.generateSessionKey(ctx, event.Source, nil)
	if err != nil {
		return nil, err
	}
	fields := pyjson.ObjectOf("user_id", optStrAny(event.UserID), "user_name", optStrAny(event.UserName), "message_id", optStrAny(event.MessageID),
		"platform_update_id", optIntAny(event.PlatformUpdateID), "reply_to_message_id", optStrAny(event.ReplyToMessageID),
		"reply_to_text", optStrAny(event.ReplyToText), "reply_to_author_id", optStrAny(event.ReplyToAuthorID),
		"reply_to_author_name", optStrAny(event.ReplyToAuthorName), "reply_to_is_own_message", event.ReplyToIsOwnMessage,
		"allow_gateway_control", event.AllowGatewayControl)
	envelope := pyjson.ObjectOf("source", encodedSource, "route", route, "event", fields, "timestamp", eventISOFormat(event.Timestamp))
	if event.Source.Platform == PlatformWebhook {
		adapter, err := runner.AdapterForSource(ctx, event.Source)
		if err != nil {
			return nil, err
		}
		if provenance == nil {
			return nil, runtimeRefusal("permission_denied")
		}
		delivery, err := webhookSnapshotDestination(ctx, adapter, event.Source.ChatID)
		if err != nil {
			return nil, err
		}
		envelope.Set("webhook_delivery", delivery)
		digest, err := webhookRouteDigest(ctx, adapter, event.Source.ChatID)
		if err != nil {
			return nil, err
		}
		envelope.Set("webhook_route", digest)
	}
	if event.Source.RoleAuthorized {
		envelope.Set("reauthorize", "roles")
	}
	if provenance != nil {
		envelope.Set("provenance", provenance)
	}
	if event.AutoSkill != nil {
		skill := event.AutoSkill
		if names, isNames := skill.([]string); isNames {
			skill = plainList(names, func(name string) any { return name })
		}
		fields.Set("auto_skill", pyjson.Clone(skill))
	}
	for _, context := range []struct {
		name  string
		value *string
	}{{"channel_prompt", event.ChannelPrompt}, {"channel_context", event.ChannelContext}} {
		if context.value != nil {
			fields.Set(context.name, *context.value)
		}
	}
	if event.ReplyExpected != nil {
		fields.Set("reply_expected", *event.ReplyExpected)
	}
	if event.ReplyAnchorOverride != nil {
		fields.Set("reply_anchor_override", *event.ReplyAnchorOverride)
	}
	if event.EditRevision != nil {
		fields.Set("edit_revision", *event.EditRevision)
	}
	if event.InheritChannelPin {
		fields.Set("inherit_channel_pin", true)
	}
	if metadata, _ := nativeMetadata(event); metadata != nil && metadata.Len() > 0 {
		fields.Set("metadata", metadata)
	}
	if event.MessageType != MessageTypeText && event.MessageType != MessageTypeCommand {
		envelope.Set("message_type", string(event.MessageType))
	}
	if len(event.MediaURLs) > 0 {
		if media == nil {
			if media, err = captureNativeMedia(ctx, event.MediaURLs); err != nil {
				return nil, err
			}
		}
		envelope.Set("media", media)
		fields.Set("media_types", plainList(event.MediaTypes, func(kind string) any { return kind }))
		fields.Set("media_text_inlined", plainList(event.MediaTextInlined, func(inlined *bool) any {
			if inlined == nil {
				return nil
			}
			return *inlined
		}))
		named := false
		for _, name := range event.MediaFilenames {
			named = named || name != ""
		}
		if named {
			fields.Set("media_filenames", plainList(event.MediaFilenames, func(name string) any { return name }))
		}
	}
	payload := pyjson.ObjectOf("text", event.Text, "native_text_v1", envelope)
	if provenance != nil && !runner.MultiplexProfiles() {
		authority, err := activeAuthority(ctx, runner)
		if err != nil {
			return nil, err
		}
		if entry := store.LookupBySessionKey(ctx, route); entry != nil {
			legacy, _ := pyjson.Clone(payload).(*pyjson.Object)
			wrapped, _ := legacy.Get("native_text_v1")
			wrapped.(*pyjson.Object).Delete("provenance")
			if authority == nil {
				return nil, &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'db'"}
			}
			rows, err := authority.DB.ListSessionAdmissions(ctx, entry.SessionID, false)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				requestID, _ := row.Get("request_id")
				if pyjson.Equal(requestID, optStrAny(event.MessageID)) && pyjson.Equal(rowPayload(row), legacy) {
					return legacy, nil
				}
			}
		}
	}
	return payload, nil
}

// Native envelope of a committed payload
func nativeEnvelope(payload *pyjson.Object) (*pyjson.Object, error) {
	value, ok := payload.Get("native_text_v1")
	if !ok {
		return nil, &pyKeyError{"native_text_v1"}
	}
	envelope, isEnvelope := value.(*pyjson.Object)
	if !isEnvelope || envelope == nil {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(value) + "' object is not subscriptable"}
	}
	return envelope, nil
}

// Sender of a committed native payload with its provenance restored, its captured media left unverified
func restoreNativeSource(ctx context.Context, payload *pyjson.Object, runner AuthorityRunner) (*SessionSource, error) {
	envelope, err := nativeEnvelope(payload)
	if err != nil {
		return nil, err
	}
	encoded, ok := envelope.Get("source")
	if !ok {
		return nil, &pyKeyError{"source"}
	}
	wire, isWire := attachmentItem(encoded)
	if !isWire {
		return nil, &PyError{Kind: "TypeError", Msg: "'" + pyTypeName(encoded) + "' object is not subscriptable"}
	}
	source, err := SessionSourceFromDict(ctx, wire)
	if err != nil {
		return nil, err
	}
	bot, has := wire["is_bot"]
	if !has {
		return nil, &pyKeyError{"is_bot"}
	}
	source.IsBot = pyTruthy(bot)
	reauthorize, _ := envelope.Get("reauthorize")
	source.RoleAuthorized = reauthorize == "roles"
	if provenance, bound := envelope.Get("provenance"); bound {
		if runner == nil {
			runner = callbackRunner(ctx)
		}
		if runner != nil {
			if _, err := restoreProvenance(ctx, runner, source, provenance); err != nil {
				return nil, err
			}
		}
	}
	return source, nil
}

// Optional string of a committed event field
func eventOptString(name string, value any) (*string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		return &v, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: name + " must be str or None, not " + pyTypeName(value)}
}

// List of strings of a committed event field
func eventStringList(name string, value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: name + " must be list, not " + pyTypeName(value)}
	}
	out := make([]string, len(items))
	for i, item := range items {
		text, isText := item.(string)
		if !isText {
			return nil, &PyError{Kind: "TypeError", Msg: name + " items must be str, not " + pyTypeName(item)}
		}
		out[i] = text
	}
	return out, nil
}

// Sets one committed keyword of an event, an unknown or repeated keyword refused as the constructor refuses it
func setEventField(event *MessageEvent, name string, value any) error {
	text := map[string]**string{"user_id": &event.UserID, "user_name": &event.UserName, "message_id": &event.MessageID,
		"ledger_message_id": &event.LedgerMessageID, "reply_anchor_override": &event.ReplyAnchorOverride,
		"reply_to_message_id": &event.ReplyToMessageID, "reply_to_text": &event.ReplyToText, "reply_to_author_id": &event.ReplyToAuthorID,
		"reply_to_author_name": &event.ReplyToAuthorName, "channel_prompt": &event.ChannelPrompt, "channel_context": &event.ChannelContext,
		"edit_revision": &event.EditRevision}
	flags := map[string]*bool{"reply_to_is_own_message": &event.ReplyToIsOwnMessage, "internal": &event.Internal,
		"allow_gateway_control": &event.AllowGatewayControl, "inherit_channel_pin": &event.InheritChannelPin}
	if field, ok := text[name]; ok {
		parsed, err := eventOptString(name, value)
		*field = parsed
		return err
	}
	if field, ok := flags[name]; ok {
		*field = pyTruthy(value)
		return nil
	}
	var err error
	switch name {
	case "platform_update_id":
		event.PlatformUpdateID = rowInt(pyjson.ObjectOf(name, value), name)
		if value != nil && event.PlatformUpdateID == nil {
			return &PyError{Kind: "TypeError", Msg: name + " must be int or None, not " + pyTypeName(value)}
		}
	case "reply_expected":
		event.ReplyExpected = nil
		if value != nil {
			expected := pyTruthy(value)
			event.ReplyExpected = &expected
		}
	case "auto_skill":
		event.AutoSkill = pyjson.Clone(value)
	case "raw_message":
		event.RawMessage = value
	case "metadata":
		event.Metadata, _ = pyjson.Plain(pyjson.Clone(value)).(map[string]any)
	case "prompt_response":
		event.PromptResponse, _ = pyjson.Plain(pyjson.Clone(value)).(map[string]any)
	case "media_types":
		event.MediaTypes, err = eventStringList(name, value)
	case "media_filenames":
		event.MediaFilenames, err = eventStringList(name, value)
	case "media_text_inlined":
		items, isList := value.([]any)
		if !isList {
			return &PyError{Kind: "TypeError", Msg: name + " must be list, not " + pyTypeName(value)}
		}
		event.MediaTextInlined = make([]*bool, len(items))
		for i, item := range items {
			if item != nil {
				inlined := pyTruthy(item)
				event.MediaTextInlined[i] = &inlined
			}
		}
	case "text", "source", "timestamp", "message_type", "media_urls":
		return &PyError{Kind: "TypeError", Msg: "gateway.platforms.event.MessageEvent() got multiple values for keyword argument '" + name + "'"}
	default:
		return &PyError{Kind: "TypeError", Msg: "MessageEvent.__init__() got an unexpected keyword argument '" + name + "'"}
	}
	return err
}

// Event of a committed native payload with its sender, context and byte verified media
func restoreNative(ctx context.Context, payload *pyjson.Object, runner AuthorityRunner) (*MessageEvent, error) {
	envelope, err := nativeEnvelope(payload)
	if err != nil {
		return nil, err
	}
	source, err := restoreNativeSource(ctx, payload, runner)
	if err != nil {
		return nil, err
	}
	stamp, ok := envelope.Get("timestamp")
	if !ok {
		return nil, &pyKeyError{"timestamp"}
	}
	stampText, isText := stamp.(string)
	if !isText {
		return nil, &PyError{Kind: "TypeError", Msg: "fromisoformat: argument must be str"}
	}
	timestamp, err := fromISOFormat(stampText)
	if err != nil {
		return nil, err
	}
	at := timestamp.T
	if timestamp.Aware && at.UnixNano() == 0 {
		at = time.Now()
	}
	text, ok := payload.Get("text")
	if !ok {
		return nil, &pyKeyError{"text"}
	}
	body, isBody := text.(string)
	if !isBody {
		return nil, &PyError{Kind: "TypeError", Msg: "native text must be str, not " + pyTypeName(text)}
	}
	kind := MessageTypeText
	if named, has := envelope.Get("message_type"); has {
		name, _ := named.(string)
		if kind = MessageType(name); !messageTypes[kind] {
			return nil, &PyError{Kind: "ValueError", Msg: pyrepr.Repr(named) + " is not a valid MessageType"}
		}
	}
	var references []any
	if media, has := envelope.Get("media"); has {
		references, _ = media.([]any)
	}
	urls, err := restoreNativeMedia(ctx, references)
	if err != nil {
		return nil, err
	}
	event := NewMessageEvent(body)
	event.Source, event.Timestamp, event.MessageType, event.MediaURLs = source, at, kind, urls
	committed, ok := envelope.Get("event")
	if !ok {
		return nil, &pyKeyError{"event"}
	}
	fields, isFields := committed.(*pyjson.Object)
	if !isFields || fields == nil {
		return nil, &PyError{Kind: "TypeError", Msg: "gateway.platforms.event.MessageEvent() argument after ** must be a mapping, not " + pyTypeName(committed)}
	}
	for _, name := range fields.Keys {
		value, _ := fields.Get(name)
		if err := setEventField(event, name, value); err != nil {
			return nil, err
		}
	}
	described, automated := envelope.Get("automation")
	if !automated {
		return event, nil
	}
	automation, isAutomation := described.(*pyjson.Object)
	if !isAutomation || automation == nil {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(described) + "' object has no attribute 'get'"}
	}
	event.Internal = true
	if heartbeat, _ := automation.Get("heartbeat"); pyTruthy(heartbeat) {
		session := pyrepr.Str(heartbeat)
		event.SetHeartbeatSessionID(&session)
	}
	route, ok := envelope.Get("route")
	if !ok {
		return nil, &pyKeyError{"route"}
	}
	owner, ok := automation.Get("owner")
	if !ok {
		return nil, &pyKeyError{"owner"}
	}
	event.Metadata = map[string]any{"gateway_session_key": route, "gateway_session_id": owner}
	for _, describe := range []func(func(string) (any, bool)) (*pyjson.Object, error){automationNotificationMetadata, automationDisplayMetadata} {
		extra, err := describe(automation.Get)
		if err != nil {
			return nil, err
		}
		for _, key := range extra.Keys {
			event.Metadata[key], _ = extra.Get(key)
		}
	}
	return event, nil
}

// Read only preflight of a queued native row, its sender, route, session and adapter checked without consuming it
func checkNativeRoute(ctx context.Context, runner AuthorityRunner, payload *pyjson.Object, sessionID string, availableSource *SessionSource, adapter PlatformAdapter) (*SessionSource, string, error) {
	envelope, err := nativeEnvelope(payload)
	if err != nil {
		return nil, "", err
	}
	if _, automated := envelope.Get("automation"); automated {
		return checkAutomationRoute(ctx, runner, payload, sessionID, availableSource, adapter)
	}
	event, err := restoreNative(ctx, payload, runner)
	if err != nil {
		return nil, "", err
	}
	provenance, _ := envelope.Get("provenance")
	freshRoles, err := reauthorizeRoles(ctx, runner, event.Source, provenance)
	if err != nil {
		return nil, "", err
	}
	if _, err := validateNative(ctx, runner, event, provenance, freshRoles); err != nil {
		return nil, "", err
	}
	committed, ok := envelope.Get("route")
	if !ok {
		return nil, "", &pyKeyError{"route"}
	}
	route, _ := committed.(string)
	store := runner.SessionStore()
	for _, source := range []*SessionSource{event.Source, availableSource} {
		key, err := store.generateSessionKey(ctx, source, nil)
		if err != nil {
			return nil, "", err
		}
		if key != route {
			return nil, "", runtimeRefusal("admission_conflict")
		}
	}
	if entry := store.LookupBySessionKey(ctx, route); entry == nil || entry.SessionID != sessionID {
		return nil, "", runtimeRefusal("admission_conflict")
	}
	if adapter == nil {
		return nil, "", runtimeRefusal("not_found")
	}
	if owner, err := runner.AdapterForSource(ctx, event.Source); err != nil {
		return nil, "", err
	} else if owner != adapter {
		return nil, "", runtimeRefusal("not_found")
	}
	if event.Source.Platform == PlatformWebhook {
		delivery, _ := envelope.Get("webhook_delivery")
		if _, err := webhookValidateDestination(ctx, delivery); err != nil {
			return nil, "", err
		}
		digest, _ := envelope.Get("webhook_route")
		current, err := webhookRouteDigest(ctx, adapter, event.Source.ChatID)
		if err != nil {
			return nil, "", err
		}
		if !pyjson.Equal(digest, current) {
			return nil, "", runtimeRefusal("admission_conflict")
		}
	}
	return event.Source, route, nil
}
