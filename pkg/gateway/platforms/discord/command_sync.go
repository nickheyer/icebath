package discord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const discordCommandSyncStateFilename = "discord_command_sync_state.json"

// Pacing between command sync mutations in seconds
var commandSyncMutationInterval = 4.5

var discordCommandSyncPolicies = map[string]bool{"safe": true, "bulk": true, "off": true}

// Value decoded from JSON with integral numbers as int64 the way Python json loads them
func jsonValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return intifyNumbers(out), nil
}

func intifyNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		for k, x := range t {
			t[k] = intifyNumbers(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = intifyNumbers(x)
		}
		return t
	}
	return v
}

// Path of the slash command sync state file, its directory created on demand
func commandSyncStatePath(ctx context.Context) string {
	directory := filepath.Join(home.Dir(ctx), discordCommandSyncStateSubdir)
	os.MkdirAll(directory, 0o755)
	return filepath.Join(directory, discordCommandSyncStateFilename)
}

// Persisted sync state, empty when missing, unreadable or not an object
func readCommandSyncState(ctx context.Context) map[string]any {
	raw, err := os.ReadFile(commandSyncStatePath(ctx))
	if err != nil {
		return map[string]any{}
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return map[string]any{}
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return map[string]any{}
	}
	if obj, ok := data.(*pyjson.Object); ok {
		out := map[string]any{}
		for _, k := range obj.Keys {
			v, _ := obj.Get(k)
			out[k] = pyjsonPlain(v)
		}
		return out
	}
	return map[string]any{}
}

// Plain Go maps and slices of a pyjson value
func pyjsonPlain(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := map[string]any{}
		for _, k := range t.Keys {
			x, _ := t.Get(k)
			out[k] = pyjsonPlain(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = pyjsonPlain(x)
		}
		return out
	}
	return v
}

// Writes the sync state compactly
func writeCommandSyncState(ctx context.Context, st map[string]any) error {
	return config.AtomicJSONWriteWith(ctx, commandSyncStatePath(ctx), st, pyjson.Separators(",", ":"))
}

// State key of an application id
func commandSyncStateKey(appID string) string {
	if appID == "" {
		return "unknown"
	}
	return appID
}

// Canonical sorted desired payloads hashed with the active language
func (a *DiscordAdapter) desiredCommandSyncFingerprint(ctx context.Context) string {
	desired := []any{}
	for _, payload := range a.desiredCommands {
		desired = append(desired, canonicalizeAppCommandPayload(payload))
	}
	sort.SliceStable(desired, func(i, j int) bool {
		x, y := desired[i].(map[string]any), desired[j].(map[string]any)
		if x["type"].(int64) != y["type"].(int64) {
			return x["type"].(int64) < y["type"].(int64)
		}
		return x["name"].(string) < y["name"].(string)
	})
	payload, _ := pyjson.Dumps(map[string]any{"language": i18n.GetLanguage(ctx), "commands": desired}, pyjson.SortKeys(true), pyjson.Separators(",", ":"))
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Python float of a state value, zero when falsy or unparsable
func stateFloat(v any) float64 {
	if !pyrepr.Truthy(v) {
		return 0
	}
	f, err := pyint.Float(v)
	if err != nil {
		return 0
	}
	return f
}

// Reason to skip the sync, a pending rate limit or an already synced fingerprint, empty otherwise
func (a *DiscordAdapter) commandSyncSkipReason(ctx context.Context, appID, fingerprint string) string {
	entry, ok := readCommandSyncState(ctx)[commandSyncStateKey(appID)].(map[string]any)
	if !ok {
		return ""
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if until := stateFloat(entry["retry_after_until"]); until > now {
		remaining := max(1, int(until-now))
		return fmt.Sprintf("Discord asked us to wait before syncing slash commands; retry in %ds", remaining)
	}
	lastSuccess, lastAttempt := stateFloat(entry["last_success_at"]), stateFloat(entry["last_attempt_at"])
	if entry["fingerprint"] == fingerprint && lastSuccess != 0 && lastSuccess >= lastAttempt {
		return "same slash-command fingerprint already synced"
	}
	return ""
}

// Rewrites this app's sync state entry, merged over the existing one when asked
func (a *DiscordAdapter) updateCommandSyncEntry(ctx context.Context, appID, fingerprint string, keepExisting bool, drop []string, fields func(now float64) map[string]any) {
	st := readCommandSyncState(ctx)
	key := commandSyncStateKey(appID)
	entry := map[string]any{}
	if existing, ok := st[key].(map[string]any); ok && keepExisting {
		for k, v := range existing {
			entry[k] = v
		}
	}
	for _, name := range drop {
		delete(entry, name)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	entry["fingerprint"] = fingerprint
	entry["last_attempt_at"] = now
	if fields != nil {
		for k, v := range fields(now) {
			entry[k] = v
		}
	}
	st[key] = entry
	if err := writeCommandSyncState(ctx, st); err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to write Discord command sync state: %s", a.Name(), err))
	}
}

// Records a sync attempt, clearing the last success
func (a *DiscordAdapter) recordCommandSyncAttempt(ctx context.Context, appID, fingerprint string) {
	a.updateCommandSyncEntry(ctx, appID, fingerprint, true, []string{"last_success_at", "summary"}, nil)
}

// Records the rate limit window Discord asked for, at least one second
func (a *DiscordAdapter) recordCommandSyncRateLimit(ctx context.Context, appID, fingerprint string, retryAfter float64) {
	retryAfter = math.Max(1.0, retryAfter)
	a.updateCommandSyncEntry(ctx, appID, fingerprint, true, nil, func(float64) map[string]any {
		return map[string]any{"retry_after_until": float64(time.Now().UnixNano())/1e9 + retryAfter, "retry_after": retryAfter}
	})
}

// Records a successful sync with its summary as a fresh entry
func (a *DiscordAdapter) recordCommandSyncSuccess(ctx context.Context, appID, fingerprint string, summary map[string]int) {
	a.updateCommandSyncEntry(ctx, appID, fingerprint, false, nil, func(float64) map[string]any {
		return map[string]any{"last_success_at": float64(time.Now().UnixNano()) / 1e9, "summary": summary}
	})
}

// Whether the error is a Discord 429 rate limit
func isDiscordRateLimit(err error) bool {
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusTooManyRequests {
		return true
	}
	var exc *discordHTTPException
	return errors.As(err, &exc) && exc.Status == http.StatusTooManyRequests
}

// Whether the error is Discord's expired interaction token error
func isDiscordUnknownInteraction(err error) bool {
	code, status := 0, 0
	var rest *discordgo.RESTError
	var exc *discordHTTPException
	switch {
	case errors.As(err, &exc):
		code, status = exc.Code, exc.Status
	case errors.As(err, &rest):
		if rest.Message != nil {
			code = rest.Message.Code
		}
		if rest.Response != nil {
			status = rest.Response.StatusCode
		}
	}
	return code == 10062 || (status == 404 && strings.Contains(strings.ToLower(err.Error()), "unknown interaction"))
}

// Seconds between two command sync mutations
func (a *DiscordAdapter) commandSyncMutationIntervalSeconds() float64 {
	return commandSyncMutationInterval
}

// Sleeps the mutation interval between command sync writes
func (a *DiscordAdapter) sleepBetweenCommandSyncMutations(ctx context.Context) error {
	interval := a.commandSyncMutationIntervalSeconds()
	if interval <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(interval * float64(time.Second))):
		return nil
	}
}

// Configured sync policy of safe, bulk or off, safe on an invalid value
func (a *DiscordAdapter) getDiscordCommandSyncPolicy(ctx context.Context) string {
	raw := strings.ToLower(gateway.PlatformGateEnv(ctx, "DISCORD_COMMAND_SYNC_POLICY", "safe"))
	if discordCommandSyncPolicies[raw] {
		return raw
	}
	if raw != "" {
		slog.Warn(fmt.Sprintf("[%s] Invalid DISCORD_COMMAND_SYNC_POLICY=%s; falling back to 'safe'", a.Name(), pyrepr.Repr(raw)))
	}
	return "safe"
}

func intOr(v any, def int64) int64 {
	if !pyrepr.Truthy(v) {
		return def
	}
	n, err := pyint.Of(v)
	if err != nil || !n.IsInt64() {
		return def
	}
	return n.Int64()
}

func strOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

func boolOr(m map[string]any, key string, def bool) bool {
	v, ok := m[key]
	if !ok {
		return def
	}
	return pyrepr.Truthy(v)
}

func sortedInts(v any) any {
	items, _ := v.([]any)
	if len(items) == 0 {
		return nil
	}
	out := make([]int64, 0, len(items))
	for _, x := range items {
		out = append(out, intOr(x, 0))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	res := make([]any, len(out))
	for i, x := range out {
		res[i] = x
	}
	return res
}

func dictItems(v any) []map[string]any {
	items, _ := v.([]any)
	var out []map[string]any
	for _, x := range items {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Command payload reduced to the semantic fields the adapter manages
func canonicalizeAppCommandPayload(payload map[string]any) map[string]any {
	typ := int64(1)
	if v, ok := payload["type"]; ok {
		typ = intOr(v, 1)
	}
	options := []any{}
	for _, item := range dictItems(payload["options"]) {
		options = append(options, canonicalizeAppCommandOption(item))
	}
	return map[string]any{
		"type": typ, "name": strOr(payload["name"]), "description": strOr(payload["description"]),
		"default_member_permissions": normalizePermissions(payload["default_member_permissions"]),
		"dm_permission":              boolOr(payload, "dm_permission", true), "nsfw": boolOr(payload, "nsfw", false),
		"contexts": sortedInts(payload["contexts"]), "integration_types": sortedInts(payload["integration_types"]),
		"options": options,
	}
}

// Permission bits as a string or nil, Discord returning strings where payloads carry ints
func normalizePermissions(value any) any {
	if value == nil {
		return nil
	}
	return pyrepr.Str(value)
}

// Payload of a fetched command with the attributes its dict form omits
func existingCommandToPayload(cmd *discordgo.ApplicationCommand) (map[string]any, error) {
	v, err := jsonValue(cmd)
	if err != nil {
		return nil, err
	}
	payload, _ := v.(map[string]any)
	if cmd.NSFW != nil {
		payload["nsfw"] = *cmd.NSFW
	}
	payload["dm_permission"] = cmd.DMPermission == nil || *cmd.DMPermission
	if cmd.DefaultMemberPermissions != nil {
		payload["default_member_permissions"] = *cmd.DefaultMemberPermissions
	}
	return payload, nil
}

// Option payload reduced to its semantic fields
func canonicalizeAppCommandOption(payload map[string]any) map[string]any {
	choices := []any{}
	for _, c := range dictItems(payload["choices"]) {
		choices = append(choices, map[string]any{"name": strOr(c["name"]), "value": c["value"]})
	}
	channelTypes := []any{}
	if items, ok := payload["channel_types"].([]any); ok {
		channelTypes = append(channelTypes, items...)
	}
	options := []any{}
	for _, item := range dictItems(payload["options"]) {
		options = append(options, canonicalizeAppCommandOption(item))
	}
	typ := int64(0)
	if v, ok := payload["type"]; ok {
		typ = intOr(v, 0)
	}
	return map[string]any{
		"type": typ, "name": strOr(payload["name"]), "description": strOr(payload["description"]),
		"required": boolOr(payload, "required", false), "autocomplete": boolOr(payload, "autocomplete", false),
		"choices": choices, "channel_types": channelTypes,
		"min_value": payload["min_value"], "max_value": payload["max_value"],
		"min_length": payload["min_length"], "max_length": payload["max_length"],
		"options": options,
	}
}

// Name, description and options, the fields the edit route accepts
func patchableAppCommandPayload(payload map[string]any) map[string]any {
	c := canonicalizeAppCommandPayload(payload)
	return map[string]any{"name": c["name"], "description": c["description"], "options": c["options"]}
}

// Application id of the live client, the bot user id when the application is unknown
func applicationID(client *discordgo.Session) string {
	if client.State != nil && client.State.Application != nil && client.State.Application.ID != "" {
		return client.State.Application.ID
	}
	if client.State != nil && client.State.User != nil {
		return client.State.User.ID
	}
	return ""
}

// Diffs existing global commands against the desired set and mutates only what changed
func (a *DiscordAdapter) safeSyncSlashCommands(ctx context.Context) (map[string]int, error) {
	summary := map[string]int{"total": 0, "unchanged": 0, "updated": 0, "recreated": 0, "created": 0, "deleted": 0}
	client := a.liveClient()
	if client == nil {
		return summary, nil
	}
	appID := applicationID(client)
	if appID == "" {
		return nil, errors.New("Discord application ID is unavailable for slash command sync")
	}
	type key struct {
		typ  int64
		name string
	}
	var desiredKeys []key
	desiredByKey := map[key]map[string]any{}
	for _, payload := range a.desiredCommands {
		k := key{intOr(payload["type"], 1), strings.ToLower(strOr(payload["name"]))}
		if _, seen := desiredByKey[k]; !seen {
			desiredKeys = append(desiredKeys, k)
		}
		desiredByKey[k] = payload
	}
	existing, err := client.ApplicationCommands(appID, "", discordgo.WithContext(ctx))
	if err != nil {
		return nil, dpyError(err)
	}
	var existingKeys []key
	existingByKey := map[key]*discordgo.ApplicationCommand{}
	for _, cmd := range existing {
		typ := int64(cmd.Type)
		if typ == 0 {
			typ = 1
		}
		k := key{typ, strings.ToLower(cmd.Name)}
		if _, seen := existingByKey[k]; !seen {
			existingKeys = append(existingKeys, k)
		}
		existingByKey[k] = cmd
	}
	mutations := 0
	mutate := func(method, endpoint string, body any) error {
		if mutations > 0 {
			if err := a.sleepBetweenCommandSyncMutations(ctx); err != nil {
				return err
			}
		}
		if _, err := client.RequestWithBucketID(method, endpoint, body, endpoint, discordgo.WithContext(ctx)); err != nil {
			return dpyError(err)
		}
		mutations++
		return nil
	}
	for _, k := range existingKeys {
		if _, wanted := desiredByKey[k]; wanted {
			continue
		}
		current := existingByKey[k]
		delete(existingByKey, k)
		if err := mutate(http.MethodDelete, discordgo.EndpointApplicationGlobalCommand(appID, current.ID), nil); err != nil {
			return nil, err
		}
		summary["deleted"]++
	}
	for _, k := range desiredKeys {
		desired := desiredByKey[k]
		current, ok := existingByKey[k]
		delete(existingByKey, k)
		if !ok {
			if err := mutate(http.MethodPost, discordgo.EndpointApplicationGlobalCommands(appID), desired); err != nil {
				return nil, err
			}
			summary["created"]++
			continue
		}
		currentPayload, err := existingCommandToPayload(current)
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(canonicalizeAppCommandPayload(currentPayload), canonicalizeAppCommandPayload(desired)) {
			summary["unchanged"]++
			continue
		}
		if reflect.DeepEqual(patchableAppCommandPayload(currentPayload), patchableAppCommandPayload(desired)) {
			if err := mutate(http.MethodPost, discordgo.EndpointApplicationGlobalCommands(appID), desired); err != nil {
				return nil, err
			}
			summary["recreated"]++
			continue
		}
		patch := map[string]any{}
		for _, field := range []string{"name", "description", "options"} {
			if v, ok := desired[field]; ok {
				patch[field] = v
			}
		}
		if err := mutate(http.MethodPatch, discordgo.EndpointApplicationGlobalCommand(appID, current.ID), patch); err != nil {
			return nil, err
		}
		summary["updated"]++
	}
	summary["total"] = len(a.desiredCommands)
	return summary, nil
}
