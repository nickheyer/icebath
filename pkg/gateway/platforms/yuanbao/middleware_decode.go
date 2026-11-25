package yuanbao

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// DecodeMiddleware turns the raw inbound frames, JSON or protobuf, into the push of the context
type DecodeMiddleware struct{ middlewareBase }

func newDecodeMiddleware() InboundMiddleware {
	return &DecodeMiddleware{middlewareBase{"DecodeMiddleware", "decode"}}
}

// JSON message body with either key casing normalized to msg_type and msg_content entries
func convertJSONMsgBody(rawBody any) ([]any, error) {
	result := []any{}
	if !anyutil.IsTrue(rawBody) {
		return result, nil
	}
	items, err := iterOf(rawBody)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		d, ok := asDict(item)
		if !ok {
			continue
		}
		msgType := d["msg_type"]
		if !anyutil.IsTrue(msgType) {
			msgType = dictGetOr(d, "MsgType", "")
		}
		msgContent := d["msg_content"]
		if !anyutil.IsTrue(msgContent) {
			msgContent = dictGetOr(d, "MsgContent", jsonutil.NewObject())
		}
		if text, isStr := msgContent.(string); isStr {
			if loaded, err := jsonutil.Decode(text); err == nil {
				msgContent = loaded
			} else {
				msgContent = jsonutil.ObjectOf("text", text)
			}
		}
		if !anyutil.IsTrue(msgContent) {
			msgContent = jsonutil.NewObject()
		}
		result = append(result, jsonutil.ObjectOf("msg_type", msgType, "msg_content", msgContent))
	}
	return result, nil
}

// Sender account and group code under either the Tencent IM PascalCase keys or the internal snake_case keys
func jsonSenderFields(rawJSON map[string]any) (any, any) {
	fromAccount := anyutil.Or(dictGetOr(rawJSON, "from_account", ""), dictGetOr(rawJSON, "From_Account", ""))
	groupCode := anyutil.Or(anyutil.Or(dictGetOr(rawJSON, "group_code", ""), dictGetOr(rawJSON, "GroupId", "")),
		dictGetOr(rawJSON, "group_id", ""))
	return fromAccount, groupCode
}

// JSON push shaped like a decoded protobuf push, nil for an empty push or one with no sender, body or callback command
func parseJSONPush(rawJSON map[string]any) (*jsonutil.Object, error) {
	if len(rawJSON) == 0 {
		return nil, nil
	}
	fromAccount, groupCode := jsonSenderFields(rawJSON)
	msgBody, err := convertJSONMsgBody(anyutil.Or(dictGetOr(rawJSON, "msg_body", []any{}), dictGetOr(rawJSON, "MsgBody", []any{})))
	if err != nil {
		return nil, err
	}
	if !anyutil.IsTrue(fromAccount) && len(msgBody) == 0 && !anyutil.IsTrue(rawJSON["callback_command"]) {
		return nil, nil
	}
	or := func(first string, firstDef any, second string, secondDef any) any {
		return anyutil.Or(dictGetOr(rawJSON, first, firstDef), dictGetOr(rawJSON, second, secondDef))
	}
	var recallList any
	if anyutil.IsTrue(rawJSON["recall_msg_seq_list"]) {
		recallList = rawJSON["recall_msg_seq_list"]
	}
	var traceID any = ""
	if logExt, isDict := asDict(rawJSON["log_ext"]); isDict {
		traceID = dictGetOr(logExt, "trace_id", "")
	}
	return jsonutil.ObjectOf(
		"callback_command", dictGetOr(rawJSON, "callback_command", ""),
		"from_account", fromAccount,
		"to_account", or("to_account", "", "To_Account", ""),
		"sender_nickname", or("sender_nickname", "", "nick_name", ""),
		"group_code", groupCode,
		"group_name", dictGetOr(rawJSON, "group_name", ""),
		"msg_seq", or("msg_seq", int64(0), "MsgSeq", int64(0)),
		"msg_id", anyutil.Or(or("msg_id", "", "msg_key", ""), dictGetOr(rawJSON, "MsgKey", "")),
		"msg_body", msgBody,
		"cloud_custom_data", or("cloud_custom_data", "", "CloudCustomData", ""),
		"bot_owner_id", or("bot_owner_id", "", "botOwnerId", ""),
		"recall_msg_seq_list", recallList,
		"trace_id", traceID,
	), nil
}

// Push of one raw frame and the format it decoded from, nil when the frame holds no valid message
func decodeSingle(data []byte) (*jsonutil.Object, string, error) {
	var connJSON any
	if text, err := textutil.DecodeUTF8(data); err == nil {
		if loaded, err := jsonutil.Decode(text); err == nil {
			connJSON = loaded
		}
	}
	if rawJSON, isDict := asDict(connJSON); isDict {
		push, err := parseJSONPush(rawJSON)
		if err != nil {
			return nil, "", err
		}
		if push != nil && push.Len() > 0 {
			return push, "json", nil
		}
		return nil, "", nil
	}
	if push := DecodeInboundPush(data); push != nil && push.Len() > 0 {
		return push, "protobuf", nil
	}
	return nil, "", nil
}

// Decodes every buffered frame and merges later message bodies into the first push, stopping when none decodes
func (m *DecodeMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if len(ic.RawFrames) == 0 {
		return nil
	}
	name := ic.Adapter.Name()
	var merged *jsonutil.Object
	for _, data := range ic.RawFrames {
		push, via, err := decodeSingle(data)
		if err != nil {
			return err
		}
		switch {
		case push == nil:
			head := "(empty)"
			if len(data) > 0 {
				head = hex.EncodeToString(data)
				head = head[:min(128, len(head))]
			}
			pkgLog().Info(fmt.Sprintf("[%s] Push decoded but no valid message. raw hex(first64)=%s", name, head))
		case merged == nil:
			merged, ic.DecodedVia = push, via
			pkgLog().Info(fmt.Sprintf("[%s] Frame decoded (via=%s): len=%d", name, via, len(data)))
		case anyutil.IsTrue(push.ValueOr("msg_body", []any{})):
			extra, isList := push.ValueOr("msg_body", []any{}).([]any)
			base, baseIsList := merged.ValueOr("msg_body", []any{}).([]any)
			if !isList || !baseIsList {
				return &kindErr{"TypeError", fmt.Sprintf("msg_body must be a list, got %s",
					errchain.Kind(push.ValueOr("msg_body", []any{})))}
			}
			body := append(append(append([]any{}, base...), textElem("\n")), extra...)
			merged.Set("msg_body", body)
			pkgLog().Info(fmt.Sprintf("[%s] Merged %d extra msg_body elements from aggregated push", name, len(extra)))
		}
	}
	if merged == nil || merged.Len() == 0 {
		return nil
	}
	ic.Push = merged
	body, _ := merged.ValueOr("msg_body", []any{}).([]any)
	msgTypes := make([]any, 0, len(body))
	for _, elem := range body {
		d, err := dictOf(elem)
		if err != nil {
			return err
		}
		msgTypes = append(msgTypes, dictGetOr(d, "msg_type", ""))
	}
	pkgLog().Info(fmt.Sprintf("[%s] Push decoded (via=%s): from=%s group=%s msg_id=%s msg_types=%s", name, ic.DecodedVia,
		anyutil.String(merged.ValueOr("from_account", "")), anyutil.String(merged.ValueOr("group_code", "")),
		anyutil.String(merged.ValueOr("msg_id", "")), anyutil.Quote(msgTypes)))
	if pkgLog().Enabled(ctx, slog.LevelDebug) {
		pkgLog().Debug(fmt.Sprintf("[%s] Push payload: %s", name, anyutil.String(ic.Push)))
	}
	return next()
}

// ExtractFieldsMiddleware copies the common push fields onto the context
type ExtractFieldsMiddleware struct{ middlewareBase }

func newExtractFieldsMiddleware() InboundMiddleware {
	return &ExtractFieldsMiddleware{middlewareBase{"ExtractFieldsMiddleware", "extract-fields"}}
}

func (m *ExtractFieldsMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	ic.FromAccount = ic.Push.ValueOr("from_account", "")
	ic.GroupCode = ic.Push.ValueOr("group_code", "")
	ic.GroupName = ic.Push.ValueOr("group_name", "")
	ic.SenderNickname = ic.Push.ValueOr("sender_nickname", "")
	ic.MsgID = ic.Push.ValueOr("msg_id", "")
	ic.CloudCustomData = ic.Push.ValueOr("cloud_custom_data", "")
	body, err := iterOf(ic.Push.ValueOr("msg_body", []any{}))
	if err != nil {
		return err
	}
	ic.MsgBody = body
	return next()
}

// DedupMiddleware drops a message whose id was already seen inside the dedup window
type DedupMiddleware struct{ middlewareBase }

func newDedupMiddleware() InboundMiddleware {
	return &DedupMiddleware{middlewareBase{"DedupMiddleware", "dedup"}}
}

func (m *DedupMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if anyutil.IsTrue(ic.MsgID) && ic.Adapter.dedup.IsDuplicate(anyutil.String(ic.MsgID)) {
		pkgLog().Debug(fmt.Sprintf("[%s] Duplicate message ignored: msg_id=%s", ic.Adapter.Name(), anyutil.String(ic.MsgID)))
		return nil
	}
	return next()
}

// SkipSelfMiddleware drops the messages the bot itself sent
type SkipSelfMiddleware struct{ middlewareBase }

func newSkipSelfMiddleware() InboundMiddleware {
	return &SkipSelfMiddleware{middlewareBase{"SkipSelfMiddleware", "skip-self"}}
}

// Whether a non empty sender account is the bot account
func isSelfReference(fromAccount, botID any) bool {
	return anyutil.IsTrue(fromAccount) && anyutil.Equal(fromAccount, botID)
}

func (m *SkipSelfMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if isSelfReference(ic.FromAccount, ic.Adapter.botIDValue()) {
		pkgLog().Debug(fmt.Sprintf("[%s] Ignoring self-sent message from %s", ic.Adapter.Name(), anyutil.String(ic.FromAccount)))
		return nil
	}
	return next()
}

// ChatRoutingMiddleware derives the chat id, type and name from the push fields
type ChatRoutingMiddleware struct{ middlewareBase }

func newChatRoutingMiddleware() InboundMiddleware {
	return &ChatRoutingMiddleware{middlewareBase{"ChatRoutingMiddleware", "chat-routing"}}
}

func (m *ChatRoutingMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if anyutil.IsTrue(ic.GroupCode) {
		ic.ChatID, ic.ChatType, ic.ChatName = "group:"+anyutil.String(ic.GroupCode), "group", anyutil.Or(ic.GroupName, ic.GroupCode)
	} else {
		ic.ChatID, ic.ChatType, ic.ChatName = "direct:"+anyutil.String(ic.FromAccount), "dm", anyutil.Or(ic.SenderNickname, ic.FromAccount)
	}
	return next()
}

// AccessPolicy holds the DM and group access rules shared by the inbound guard and outbound DMs
type AccessPolicy struct{ *gateway.OwnAccessPolicyMixin }

// Policy over the DM and group policies with their allowlists
func NewAccessPolicy(dmPolicy string, dmAllowFrom []string, groupPolicy string, groupAllowFrom []string) (*AccessPolicy, error) {
	mixin, err := gateway.NewOwnAccessPolicyMixin("AccessPolicy", "YUANBAO")
	if err != nil {
		return nil, err
	}
	mixin.DMPolicy, mixin.GroupPolicy = dmPolicy, groupPolicy
	for _, entry := range dmAllowFrom {
		mixin.AllowFrom[entry] = true
	}
	for _, entry := range groupAllowFrom {
		mixin.GroupAllowFrom[entry] = true
	}
	return &AccessPolicy{mixin}, nil
}

// Strict DM authorization of the stripped sender, pairing never implying access
func (p *AccessPolicy) IsDMAllowed(ctx context.Context, senderID string) (bool, error) {
	return p.OwnAccessPolicyMixin.IsDMAllowed(ctx, strings.TrimSpace(senderID))
}

// Whether a DM may reach gateway intake, the pairing handshake path included
func (p *AccessPolicy) IsDMIntakeAllowed(ctx context.Context, senderID string) (bool, error) {
	return p.OwnAccessPolicyMixin.IsDMIntakeAllowed(ctx, senderID)
}

// Whether a group reaches intake, an open group still needing the allow all opt in
func (p *AccessPolicy) IsGroupAllowed(ctx context.Context, groupCode any) (bool, error) {
	if p.GroupPolicy == "open" {
		return p.OpenDMOptedIn(ctx)
	}
	code, err := strMethod(groupCode, "the group code")
	if err != nil {
		return false, err
	}
	return p.OwnAccessPolicyMixin.IsGroupAllowed(ctx, strings.TrimSpace(code), "")
}

// AccessGuardMiddleware is the platform level DM and group access filter
type AccessGuardMiddleware struct{ middlewareBase }

func newAccessGuardMiddleware() InboundMiddleware {
	return &AccessGuardMiddleware{middlewareBase{"AccessGuardMiddleware", "access-guard"}}
}

func (m *AccessGuardMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	policy := ic.Adapter.accessPolicy
	if ic.ChatType == "dm" {
		allowed, err := policy.IsDMIntakeAllowed(ctx, strOrEmpty(ic.FromAccount))
		if err != nil {
			return err
		}
		if !allowed {
			pkgLog().Debug(fmt.Sprintf("[%s] DM from %s blocked by dm_policy=%s", ic.Adapter.Name(), anyutil.String(ic.FromAccount), policy.DMPolicy))
			return nil
		}
	}
	if ic.ChatType == "group" {
		allowed, err := policy.IsGroupAllowed(ctx, ic.GroupCode)
		if err != nil {
			return err
		}
		if !allowed {
			pkgLog().Debug(fmt.Sprintf("[%s] Group %s blocked by group_policy=%s", ic.Adapter.Name(), anyutil.String(ic.GroupCode), policy.GroupPolicy))
			return nil
		}
	}
	return next()
}

// AutoSetHomeMiddleware designates the first authorized conversation as home channel, a group home upgraded by the first DM
type AutoSetHomeMiddleware struct{ middlewareBase }

func newAutoSetHomeMiddleware() InboundMiddleware {
	return &AutoSetHomeMiddleware{middlewareBase{"AutoSetHomeMiddleware", "auto-sethome"}}
}

func (m *AutoSetHomeMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	adapter := ic.Adapter
	if !adapter.autoSethomeDoneValue() {
		may, err := adapter.senderMayDesignateHome(ctx, ic)
		if err != nil {
			return err
		}
		if may {
			curHome, err := gateway.GetScopedSecret(ctx, "YUANBAO_HOME_CHANNEL", "", false)
			if err != nil {
				return err
			}
			shouldSet := curHome == "" || (strings.HasPrefix(curHome, "group:") && ic.ChatType == "dm")
			if ic.ChatType == "dm" {
				adapter.setAutoSethomeDone(true)
			}
			if shouldSet {
				persistHome(ctx, adapter, ic)
			}
		}
	}
	return next()
}

// Writes the home channel to config.yaml, the live platform config and the unscoped process env, a failure only warned
func persistHome(ctx context.Context, adapter *YuanbaoAdapter, ic *InboundContext) {
	err := func() error {
		home := gateway.NewHomeChannel(gateway.PlatformYuanbao, ic.ChatID, anyutil.String(anyutil.Or(ic.ChatName, "Home")))
		if err := gateway.PersistHomeChannel(ctx, home, false); err != nil {
			return err
		}
		adapter.Config.HomeChannel = home
		if !gateway.ProfileScoped(ctx) {
			if err := os.Setenv("YUANBAO_HOME_CHANNEL", ic.ChatID); err != nil {
				return err
			}
		}
		pkgLog().Info(fmt.Sprintf("[%s] Auto-sethome: designated %s (%s) as Yuanbao home channel", adapter.Name(), ic.ChatID, anyutil.String(ic.ChatName)))
		return nil
	}()
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[%s] Auto-sethome failed: %s", adapter.Name(), err.Error()))
	}
}
