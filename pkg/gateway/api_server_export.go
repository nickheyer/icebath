package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Adds a platform module's attributes to the builtin adapter table
func RegisterBuiltinPlatformModule(module string, attrs map[string]any) {
	builtinPlatformModules[module] = attrs
}

// Approval command text with secrets redacted before egress
func APIRedactApprovalCommand(ctx context.Context, text any) string {
	return redactApprovalCommand(ctx, text)
}

// Stripped request string, nil when absent or blank
func APICleanRequestString(value any) *string { return apiCleanRequestString(value) }

// Request value as a boolean with string spellings understood, the default otherwise
func APICoerceRequestBool(value any, def bool) bool { return apiCoerceRequestBool(value, def) }

// Runtime kwargs with the request overrides merged in
func APIApplyRuntimeAgentOverrides(runtimeKwargs map[string]any, overrides any) map[string]any {
	return apiApplyRuntimeAgentOverrides(runtimeKwargs, overrides)
}

// Sentinel for a request option that was not given
var APIRequestOptionMissing = apiRequestOptionMissing

// Reasoning config a request's model options ask for, nil when none
func APIRequestReasoningConfig(modelOptions any) map[string]any {
	return apiRequestReasoningConfig(modelOptions)
}

// Service tier a request's model options ask for, the missing sentinel when none
func APIRequestServiceTier(modelOptions any) any { return apiRequestServiceTier(modelOptions) }

// One SSE frame of a payload with an optional event name and id
func APISSEFrame(data any, event string, ensureASCII bool, id *int) ([]byte, error) {
	return apiSSEFrame(data, event, ensureASCII, id)
}

// Chat content normalized to plain text
func APINormalizeChatContent(content any) string { return apiNormalizeChatContent(content) }

// Multimodal content normalized for the agent, a coded error for a malformed part
func APINormalizeMultimodalContent(content any) (any, error) {
	return apiNormalizeMultimodalContent(content)
}

// Whether content carries text or media worth a turn
func APIContentHasVisiblePayload(content any) bool { return apiContentHasVisiblePayload(content) }

// Text capped at the normalized text limit
func APICapText(text string) string { return apiCapText(text) }

// Wall clock seconds of the API server clock
func APINow() float64 { return apiNow() }

// Next stable item id of a durable response
func APINextItemID(responseID string, counters map[APIItemCounterKey]int, kind string, callID any, commentary bool) (string, error) {
	return apiNextItemID(responseID, counters, kind, callID, commentary)
}

// Item counter key of a durable response
type APIItemCounterKey = apiItemCounterKey

// Output items with stable ids assigned
func APIIdentifyOutput(items []*pyjson.Object, responseID string) ([]*pyjson.Object, error) {
	return apiIdentifyOutput(items, responseID)
}

// Multiplex runtime scope of a request profile
func APIProfileScope(ctx context.Context, profile string) (context.Context, error) {
	return apiProfileScope(ctx, profile)
}

// Whether a prefix names the profile this gateway serves
func APIPrefixNamesServedProfile(ctx context.Context, profile string) bool {
	return apiPrefixNamesServedProfile(ctx, profile)
}

// Writes a platform runtime status snapshot, a failure only logged
func APIWriteRuntimeStatusSafe(ctx context.Context, b *BasePlatformAdapter, statusContext string, fields map[string]any) {
	b.writeRuntimeStatusSafe(ctx, statusContext, fields)
}

// Live session a declared conversation key names
func APIDeclaredSession(ctx context.Context, db *state.SessionDB, key string) (string, bool, error) {
	return declaredAPISession(ctx, db, key)
}

// Session authority a runtime reserved for a home, nil when none
func APIAuthorityForHome(runner any, profileHome string) (*SessionAuthority, error) {
	return authorityForHome(runner, profileHome)
}

// Whether a session id would escape its directory
func IsPathUnsafe(value string) bool { return isPathUnsafe(value, false) }

// Collects API image bytes no admission holds any more
func CollectUnheldAPIImages(ctx context.Context, db *state.SessionDB) error {
	_, err := collectUnheldAPIImages(ctx, db)
	return err
}

// Run statuses after which a replay record may be pruned
func APIRunTerminalStatus(status string) bool { return apiRunTerminalStatuses[status] }

// Execution policy and catalog advertised for one served profile
func APILocalRoomCatalog(ctx context.Context, adapter PlatformAdapter, profile, installationID string) (*pyjson.Object, *pyjson.Object, error) {
	return localRoomCatalog(ctx, adapter, profile, installationID)
}
