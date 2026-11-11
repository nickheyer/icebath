package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Admitted webhook ingress rows of a delivery chat id, automation rows on the same chat excluded
const webhookRetainedDestinationQuery = `SELECT payload_json FROM session_admissions
        WHERE json_extract(payload_json, '$.native_text_v1.source.chat_id')=?
          AND json_extract(payload_json, '$.native_text_v1.source.platform')='webhook'
          AND json_extract(payload_json, '$.native_text_v1.automation') IS NULL`

// Destination of a delivery read back from its admitted ingress, rechecked against today's connector and route
func webhookRetainedDestination(ctx context.Context, adapter PlatformAdapter, chatID string) (*pyjson.Object, error) {
	runner := adapter.Base().GatewayRunner
	if runner == nil {
		runner = handlerOwner(adapter)
	}
	if len(allAuthorities(runner)) == 0 {
		hook, err := webhookAdapterOf(adapter, "_delivery_info")
		if err != nil {
			return nil, err
		}
		return webhookValidateDestination(ctx, hook.DeliveryInfo(chatID))
	}
	candidates := servingAuthorities(runner)
	scoped, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	if scoped != nil {
		candidates = []*SessionAuthority{scoped}
	}
	var rows []*pyjson.Object
	for _, authority := range candidates {
		found, err := authority.DB.ReadAll(ctx, webhookRetainedDestinationQuery, chatID)
		if err != nil {
			return nil, err
		}
		rows = append(rows, found...)
	}
	if len(rows) != 1 {
		return nil, runtimeRefusal("not_found")
	}
	raw, _ := rows[0].Get("payload_json")
	decoded, err := pyjson.Loads(anyText(raw))
	if err != nil {
		return nil, err
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "TypeError", Msg: "list indices must be integers or slices, not str"}
	}
	rawEnvelope, found := payload.Get("native_text_v1")
	if !found {
		return nil, &pyKeyError{"native_text_v1"}
	}
	envelope, ok := rawEnvelope.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: "object has no attribute 'get'"}
	}
	authorityRunner, ok := runner.(AuthorityRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_adapter_for_source'", runner)}
	}
	event, err := restoreNative(ctx, payload, authorityRunner)
	if err != nil {
		return nil, err
	}
	if _, err := validateNative(ctx, authorityRunner, event, envelope.GetOr("provenance", nil), false); err != nil {
		return nil, err
	}
	owner, err := authorityRunner.AdapterForSource(ctx, event.Source)
	if err != nil {
		return nil, err
	}
	if owner != adapter {
		return nil, runtimeRefusal("permission_denied")
	}
	digest, err := webhookRouteDigest(ctx, adapter, chatID)
	if err != nil {
		return nil, err
	}
	if route, _ := envelope.Get("webhook_route"); route != digest {
		return nil, runtimeRefusal("admission_conflict")
	}
	return webhookValidateDestination(ctx, envelope.GetOr("webhook_delivery", nil))
}
