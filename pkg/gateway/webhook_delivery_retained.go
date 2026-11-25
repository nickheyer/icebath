package gateway

import (
	"context"
	"fmt"

	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Admitted webhook ingress rows of a delivery chat id, automation rows on the same chat excluded
const webhookRetainedDestinationQuery = `SELECT payload_json FROM session_admissions
        WHERE json_extract(payload_json, '$.native_text_v1.source.chat_id')=?
          AND json_extract(payload_json, '$.native_text_v1.source.platform')='webhook'
          AND json_extract(payload_json, '$.native_text_v1.automation') IS NULL`

// Destination of a delivery read back from its admitted ingress, rechecked against today's connector and route
func webhookRetainedDestination(ctx context.Context, adapter PlatformAdapter, chatID string) (*jsonutil.Object, error) {
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
	var rows []*jsonutil.Object
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
	decoded, err := jsonutil.Decode(anyText(raw))
	if err != nil {
		return nil, err
	}
	payload, ok := decoded.(*jsonutil.Object)
	if !ok {
		return nil, &errorutil.Error{Kind: "TypeError", Msg: "the retained webhook payload is not a mapping"}
	}
	rawEnvelope, found := payload.Get("native_text_v1")
	if !found {
		return nil, &keyError{"native_text_v1"}
	}
	envelope, ok := rawEnvelope.(*jsonutil.Object)
	if !ok {
		return nil, &errorutil.Error{Kind: "AttributeError", Msg: "the retained native text envelope is not a mapping"}
	}
	authorityRunner, ok := runner.(AuthorityRunner)
	if !ok {
		return nil, &errorutil.Error{Kind: "AttributeError", Msg: fmt.Sprintf("the gateway runner (%T) cannot find the adapter of a source", runner)}
	}
	event, err := restoreNative(ctx, payload, authorityRunner)
	if err != nil {
		return nil, err
	}
	if _, err := validateNative(ctx, authorityRunner, event, envelope.ValueOr("provenance", nil), false); err != nil {
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
	return webhookValidateDestination(ctx, envelope.ValueOr("webhook_delivery", nil))
}
