package yuanbao

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// DecodeMiddleware turns the raw inbound frames, JSON or protobuf, into the push of the context
type DecodeMiddleware struct{ middlewareBase }

func newDecodeMiddleware() InboundMiddleware {
	return &DecodeMiddleware{middlewareBase{"DecodeMiddleware", "decode"}}
}

// JSON message body with either key casing normalized to msg_type and msg_content entries
func convertJSONMsgBody(rawBody any) ([]any, error) {
	result := []any{}
	if !pyrepr.Truthy(rawBody) {
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
		if !pyrepr.Truthy(msgType) {
			msgType = dictGetOr(d, "MsgType", "")
		}
		msgContent := d["msg_content"]
		if !pyrepr.Truthy(msgContent) {
			msgContent = dictGetOr(d, "MsgContent", pyjson.NewObject())
		}
		if text, isStr := msgContent.(string); isStr {
			if loaded, err := pyjson.Loads(text); err == nil {
				msgContent = loaded
			} else {
				msgContent = pyjson.ObjectOf("text", text)
			}
		}
		if !pyrepr.Truthy(msgContent) {
			msgContent = pyjson.NewObject()
		}
		result = append(result, pyjson.ObjectOf("msg_type", msgType, "msg_content", msgContent))
	}
	return result, nil
}

// Sender account and group code under either the Tencent IM PascalCase keys or the internal snake_case keys
func jsonSenderFields(rawJSON map[string]any) (any, any) {
	fromAccount := pyrepr.Or(dictGetOr(rawJSON, "from_account", ""), dictGetOr(rawJSON, "From_Account", ""))
	groupCode := pyrepr.Or(pyrepr.Or(dictGetOr(rawJSON, "group_code", ""), dictGetOr(rawJSON, "GroupId", "")),
		dictGetOr(rawJSON, "group_id", ""))
	return fromAccount, groupCode
}

// JSON push shaped like a decoded protobuf push, nil for an empty push or one with no sender, body or callback command
func parseJSONPush(rawJSON map[string]any) (*pyjson.Object, error) {
	if len(rawJSON) == 0 {
		return nil, nil
	}
	fromAccount, groupCode := jsonSenderFields(rawJSON)
	msgBody, err := convertJSONMsgBody(pyrepr.Or(dictGetOr(rawJSON, "msg_body", []any{}), dictGetOr(rawJSON, "MsgBody", []any{})))
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(fromAccount) && len(msgBody) == 0 && !pyrepr.Truthy(rawJSON["callback_command"]) {
		return nil, nil
	}
	or := func(first string, firstDef any, second string, secondDef any) any {
		return pyrepr.Or(dictGetOr(rawJSON, first, firstDef), dictGetOr(rawJSON, second, secondDef))
	}
	var recallList any
	if pyrepr.Truthy(rawJSON["recall_msg_seq_list"]) {
		recallList = rawJSON["recall_msg_seq_list"]
	}
	var traceID any = ""
	if logExt, isDict := asDict(rawJSON["log_ext"]); isDict {
		traceID = dictGetOr(logExt, "trace_id", "")
	}
	return pyjson.ObjectOf(
		"callback_command", dictGetOr(rawJSON, "callback_command", ""),
		"from_account", fromAccount,
		"to_account", or("to_account", "", "To_Account", ""),
		"sender_nickname", or("sender_nickname", "", "nick_name", ""),
		"group_code", groupCode,
		"group_name", dictGetOr(rawJSON, "group_name", ""),
		"msg_seq", or("msg_seq", int64(0), "MsgSeq", int64(0)),
		"msg_id", pyrepr.Or(or("msg_id", "", "msg_key", ""), dictGetOr(rawJSON, "MsgKey", "")),
		"msg_body", msgBody,
		"cloud_custom_data", or("cloud_custom_data", "", "CloudCustomData", ""),
		"bot_owner_id", or("bot_owner_id", "", "botOwnerId", ""),
		"recall_msg_seq_list", recallList,
		"trace_id", traceID,
	), nil
}

// Push of one raw frame and the format it decoded from, nil when the frame holds no valid message
func decodeSingle(data []byte) (*pyjson.Object, string, error) {
	var connJSON any
	if text, err := pystr.DecodeUTF8(data); err == nil {
		if loaded, err := pyjson.Loads(text); err == nil {
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
	var merged *pyjson.Object
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
			slog.Info(fmt.Sprintf("[%s] Push decoded but no valid message. raw hex(first64)=%s", name, head))
		case merged == nil:
			merged, ic.DecodedVia = push, via
			slog.Info(fmt.Sprintf("[%s] Frame decoded (via=%s): len=%d", name, via, len(data)))
		case pyrepr.Truthy(push.GetOr("msg_body", []any{})):
			extra, isList := push.GetOr("msg_body", []any{}).([]any)
			base, baseIsList := merged.GetOr("msg_body", []any{}).([]any)
			if !isList || !baseIsList {
				return &pyErr{"TypeError", fmt.Sprintf("can only concatenate list (not \"%s\") to list",
					pyrepr.TypeName(push.GetOr("msg_body", []any{})))}
			}
			body := append(append(append([]any{}, base...), textElem("\n")), extra...)
			merged.Set("msg_body", body)
			slog.Info(fmt.Sprintf("[%s] Merged %d extra msg_body elements from aggregated push", name, len(extra)))
		}
	}
	if merged == nil || merged.Len() == 0 {
		return nil
	}
	ic.Push = merged
	body, _ := merged.GetOr("msg_body", []any{}).([]any)
	msgTypes := make([]any, 0, len(body))
	for _, elem := range body {
		d, err := dictOf(elem)
		if err != nil {
			return err
		}
		msgTypes = append(msgTypes, dictGetOr(d, "msg_type", ""))
	}
	slog.Info(fmt.Sprintf("[%s] Push decoded (via=%s): from=%s group=%s msg_id=%s msg_types=%s", name, ic.DecodedVia,
		pyrepr.Str(merged.GetOr("from_account", "")), pyrepr.Str(merged.GetOr("group_code", "")),
		pyrepr.Str(merged.GetOr("msg_id", "")), pyrepr.Repr(msgTypes)))
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		slog.Debug(fmt.Sprintf("[%s] Push payload: %s", name, pyrepr.Str(ic.Push)))
	}
	return next()
}

// ExtractFieldsMiddleware copies the common push fields onto the context
type ExtractFieldsMiddleware struct{ middlewareBase }

func newExtractFieldsMiddleware() InboundMiddleware {
	return &ExtractFieldsMiddleware{middlewareBase{"ExtractFieldsMiddleware", "extract-fields"}}
}

func (m *ExtractFieldsMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	ic.FromAccount = ic.Push.GetOr("from_account", "")
	ic.GroupCode = ic.Push.GetOr("group_code", "")
	ic.GroupName = ic.Push.GetOr("group_name", "")
	ic.SenderNickname = ic.Push.GetOr("sender_nickname", "")
	ic.MsgID = ic.Push.GetOr("msg_id", "")
	ic.CloudCustomData = ic.Push.GetOr("cloud_custom_data", "")
	body, err := iterOf(ic.Push.GetOr("msg_body", []any{}))
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
	if pyrepr.Truthy(ic.MsgID) && ic.Adapter.dedup.IsDuplicate(pyrepr.Str(ic.MsgID)) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate message ignored: msg_id=%s", ic.Adapter.Name(), pyrepr.Str(ic.MsgID)))
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
	return pyrepr.Truthy(fromAccount) && pyrepr.Equal(fromAccount, botID)
}

func (m *SkipSelfMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if isSelfReference(ic.FromAccount, ic.Adapter.botIDValue()) {
		slog.Debug(fmt.Sprintf("[%s] Ignoring self-sent message from %s", ic.Adapter.Name(), pyrepr.Str(ic.FromAccount)))
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
	if pyrepr.Truthy(ic.GroupCode) {
		ic.ChatID, ic.ChatType, ic.ChatName = "group:"+pyrepr.Str(ic.GroupCode), "group", pyrepr.Or(ic.GroupName, ic.GroupCode)
	} else {
		ic.ChatID, ic.ChatType, ic.ChatName = "direct:"+pyrepr.Str(ic.FromAccount), "dm", pyrepr.Or(ic.SenderNickname, ic.FromAccount)
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
	return p.OwnAccessPolicyMixin.IsDMAllowed(ctx, pystr.Strip(senderID))
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
	code, err := strMethod(groupCode, "strip")
	if err != nil {
		return false, err
	}
	return p.OwnAccessPolicyMixin.IsGroupAllowed(ctx, pystr.Strip(code), "")
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
			slog.Debug(fmt.Sprintf("[%s] DM from %s blocked by dm_policy=%s", ic.Adapter.Name(), pyrepr.Str(ic.FromAccount), policy.DMPolicy))
			return nil
		}
	}
	if ic.ChatType == "group" {
		allowed, err := policy.IsGroupAllowed(ctx, ic.GroupCode)
		if err != nil {
			return err
		}
		if !allowed {
			slog.Debug(fmt.Sprintf("[%s] Group %s blocked by group_policy=%s", ic.Adapter.Name(), pyrepr.Str(ic.GroupCode), policy.GroupPolicy))
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
		home := gateway.NewHomeChannel(gateway.PlatformYuanbao, ic.ChatID, pyrepr.Str(pyrepr.Or(ic.ChatName, "Home")))
		if err := gateway.PersistHomeChannel(ctx, home, false); err != nil {
			return err
		}
		adapter.Config.HomeChannel = home
		if !gateway.ProfileScoped(ctx) {
			if err := os.Setenv("YUANBAO_HOME_CHANNEL", ic.ChatID); err != nil {
				return err
			}
		}
		slog.Info(fmt.Sprintf("[%s] Auto-sethome: designated %s (%s) as Yuanbao home channel", adapter.Name(), ic.ChatID, pyrepr.Str(ic.ChatName)))
		return nil
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Auto-sethome failed: %s", adapter.Name(), errText(err)))
	}
}
