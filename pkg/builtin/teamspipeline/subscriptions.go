package teamspipeline

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type graphClient interface {
	GetJSON(ctx context.Context, path string, params map[string]any, headers map[string]string) (any, error)
	PatchJSON(ctx context.Context, path string, jsonBody any, headers map[string]string) (any, error)
	PostJSON(ctx context.Context, path string, jsonBody any, headers map[string]string) (any, error)
	Delete(ctx context.Context, path string, headers map[string]string) (map[string]any, error)
	CollectPaginated(ctx context.Context, path string, params map[string]any, headers map[string]string) ([]any, error)
	DownloadToFile(ctx context.Context, path, destination string, headers map[string]string) (map[string]any, error)
}

// Graph client authenticated from the MSGRAPH environment credentials
func buildGraphClient() (graphClient, error) {
	tp, err := messagetools.MicrosoftGraphTokenProviderFromEnv(nil)
	if err != nil {
		return nil, err
	}
	return messagetools.NewMicrosoftGraphClient(tp), nil
}

// Current UTC instant at microsecond precision like datetime.now
func utcNow() time.Time { return nowUTC().Truncate(time.Microsecond) }

// Second precision UTC timestamp with a Z suffix hours from base or now
func utcTimestamp(hoursFromNow int, base *time.Time) string {
	b := utcNow()
	if base != nil {
		b = *base
	}
	t := b.Add(time.Duration(hoursFromNow) * time.Hour)
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, t.Location())
	return serializeDatetime(&t).(string)
}

// Normalizes a Graph subscription into the store with an expiry derived status and renewal stamp
func syncGraphSubscriptionRecord(store *TeamsPipelineStore, payload *pyjson.Object, status string, renewed bool) (*pyjson.Object, error) {
	sub, err := GraphSubscriptionFromDict(payload)
	if err != nil {
		return nil, err
	}
	normalized := sub.ToDict()
	if status == "" {
		status = "active"
		exp, err := parseDatetime(get(normalized, "expiration_datetime"))
		if err != nil {
			return nil, err
		}
		if exp != nil && !exp.After(utcNow()) {
			status = "expired"
		}
	}
	normalized.Set("status", status)
	if renewed {
		normalized.Set("latest_renewal_at", utcTimestamp(0, nil))
	}
	return store.UpsertSubscription(pyrepr.Str(get(normalized, "subscription_id")), normalized)
}

// Configured clientState from raw or MSGRAPH_WEBHOOK_CLIENT_STATE, nil when blank
func expectedClientState(raw *string) any {
	v := ""
	if raw == nil {
		v = os.Getenv("MSGRAPH_WEBHOOK_CLIENT_STATE")
	} else {
		v = *raw
	}
	if s := pystr.Strip(v); s != "" {
		return s
	}
	return nil
}

// Whether the store knows the subscription or its clientState matches the configured one
func isManagedSubscription(store *TeamsPipelineStore, payload *pyjson.Object, expected any) bool {
	id := strField(or(get(payload, "subscription_id"), get(payload, "id")))
	if id != "" && store.GetSubscription(id) != nil {
		return true
	}
	candidate := strField(or(get(payload, "client_state"), get(payload, "clientState")))
	exp, _ := expected.(string)
	return exp != "" && candidate == exp
}

// First truthy value or the last one
func or(vals ...any) any {
	for _, v := range vals[:len(vals)-1] {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

// Syncs managed Graph subscriptions, renews those expiring within the threshold and flags locally known ones Graph dropped
func maintainGraphSubscriptions(ctx context.Context, client graphClient, store *TeamsPipelineStore, renewWithinHours, extendHours int, dryRun bool, clientState *string) (*pyjson.Object, error) {
	threshold := max(1, renewWithinHours)
	extendHours = max(1, extendHours)
	managed := expectedClientState(clientState)
	now := utcNow()
	remote, err := client.CollectPaginated(ctx, "/subscriptions", nil, nil)
	if err != nil {
		return nil, err
	}
	remoteIDs := map[string]bool{}
	synced := 0
	renewed, candidates, skipped := []any{}, []any{}, []any{}
	for _, item := range remote {
		raw, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		id := strField(get(raw, "id"))
		if id == "" {
			continue
		}
		skip := func(reason string, extra ...any) {
			skipped = append(skipped, obj(append([]any{"subscription_id", id, "reason", reason}, extra...)...))
		}
		if !isManagedSubscription(store, raw, managed) {
			skip("not_managed_by_teams_pipeline")
			continue
		}
		remoteIDs[id] = true
		if _, err := syncGraphSubscriptionRecord(store, raw, "", false); err != nil {
			skip(fmt.Sprintf("failed_to_sync_local_store: %v", err))
			continue
		}
		synced++
		expiration, err := parseDatetime(get(raw, "expirationDateTime"))
		if err != nil {
			return nil, err
		}
		if expiration == nil {
			skip("missing_expiration")
			continue
		}
		secs := int64(expiration.Sub(now) / time.Second)
		if secs < 0 {
			if _, err := store.UpsertSubscription(id, obj("status", "expired", "expiration_datetime", serializeDatetime(expiration))); err != nil {
				return nil, err
			}
			skip("already_expired", "expiration_datetime", serializeDatetime(expiration))
			continue
		}
		if secs > int64(threshold)*3600 {
			skip("not_due", "expires_in_seconds", secs)
			continue
		}
		base := now
		if expiration.After(now) {
			base = *expiration
		}
		newExpiration := utcTimestamp(extendHours, &base)
		candidate := obj("subscription_id", id, "resource", get(raw, "resource"), "current_expiration", serializeDatetime(expiration), "new_expiration", newExpiration)
		candidates = append(candidates, candidate)
		if dryRun {
			continue
		}
		patched, err := client.PatchJSON(ctx, "/subscriptions/"+id, obj("expirationDateTime", newExpiration), nil)
		if err != nil {
			return nil, err
		}
		merged := raw.Copy()
		if p, ok := patched.(*pyjson.Object); ok {
			for _, k := range p.Keys {
				merged.Set(k, p.Vals[k])
			}
		}
		merged.Set("id", id)
		merged.Set("expirationDateTime", newExpiration)
		if _, err := syncGraphSubscriptionRecord(store, merged, "active", true); err != nil {
			return nil, err
		}
		rec := candidate.Copy()
		rec.Set("result", patched)
		renewed = append(renewed, rec)
	}
	for _, id := range store.ListSubscriptions().Keys {
		if !remoteIDs[id] {
			if _, err := store.UpsertSubscription(id, obj("status", "missing_remote", "last_seen_missing_remote_at", utcTimestamp(0, nil))); err != nil {
				return nil, err
			}
		}
	}
	return obj("success", true, "dry_run", dryRun, "store_path", store.Path,
		"remote_subscription_count", int64(len(remote)), "synced_subscription_count", int64(synced),
		"candidate_count", int64(len(candidates)), "renewed_count", int64(len(renewed)),
		"threshold_hours", int64(threshold), "extend_hours", int64(extendHours),
		"candidates", candidates, "renewed", renewed, "skipped", skipped), nil
}
