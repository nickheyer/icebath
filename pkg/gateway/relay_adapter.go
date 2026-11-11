package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

func relayAdapterLog() *slog.Logger { return slog.Default().With("logger", "gateway.relay.adapter") }

// Store refusal reasons a connector delivery is acknowledged for instead of replayed
var relayPermanentRefusals = map[string]bool{"permission_denied": true, "invalid_params": true, "admission_conflict": true,
	"profile_mismatch": true, "not_found": true}

const (
	relayGoIdleOnDisconnectTimeoutS        = 2.0
	relayRevocationMonitorTeardownTimeoutS = 1.0
	relayResolvedPromptMemory              = 256
	relayDraftStateCap                     = 512
	relaySlack                             = string(PlatformSlack)
)

var relayURLRe = regexp.MustCompile(`https?://|<https?:|\]\(https?:`)

var relayTruthyFlags = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

var relayFalsyFlags = map[string]bool{"0": true, "false": true, "no": true, "off": true}

var relayExecApprovalLabels = map[string]string{"once": "✅ Approved once", "session": "✅ Approved for session",
	"always": "✅ Approved permanently", "deny": "❌ Denied"}

var relaySlashConfirmLabels = map[string]string{"once": "✅ Approved once", "always": "\U0001f512 Always approve", "cancel": "❌ Cancelled"}

// Local file the send_media lane accepted but never delivered, with the reason
type relayMediaNotDelivered struct{ reason string }

// UTF-16 code unit count, the Telegram length unit
func relayUTF16Len(text string) int { return len(utf16.Encode([]rune(text))) }

// Python character count
func relayCharLen(text string) int { return len([]rune(text)) }

// Length function of a descriptor length unit, characters for any unknown unit
func relayLenFn(unit string) func(string) int {
	if unit == "utf16" {
		return relayUTF16Len
	}
	return relayCharLen
}

// SendResult projected from a connector outbound result
func relaySendResult(result map[string]any) SendResult {
	return SendResult{Success: pyrepr.Truthy(result["success"]), MessageID: result["message_id"], Error: result["error"]}
}

// Message id and chat id of an inbound event, the message id falling back to the source
func relayEventIDs(event *MessageEvent) (*string, string) {
	messageID := event.MessageID
	if messageID == nil || *messageID == "" {
		messageID = event.Source.MessageID
	}
	return messageID, event.Source.ChatID
}

// Named profile of an agent session key, nil for the legacy main namespace and the default profile
func relayProfileFromSessionKey(sessionKey string) *string {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 2 || parts[0] != "agent" || parts[1] == "" {
		return nil
	}
	profile := state.ProfileFromSessionKeyNamespace(parts[1])
	if profile == "default" {
		return nil
	}
	return &profile
}

// RelayAdapter is the generic gateway adapter fronted by the connector, advertising its negotiated capabilities
type RelayAdapter struct {
	*BasePlatformAdapter
	transport  RelayTransportConn
	descriptor CapabilityDescriptor

	mu                     sync.Mutex
	scopeByChat            map[string]string
	dmUserByChat           map[string]string
	chatTypeByChat         map[string]string
	lastInboundTSByChat    map[string]string
	platformByChat         map[string]string
	profileByChat          map[string]string
	autoThreadByChat       *relayOrdered[[2]string]
	autoThreadWaiters      map[string]*relayEvent
	seenInbound            *relayOrderedSet
	discordChatLabels      *relayOrdered[relayChatLabels]
	discordLabelsRecorded  *relayOrdered[relayChatLabels]
	admissionLanes         map[[2]string]*adapterTask
	openDraftByChat        *relayOrderedInts
	sealedDraftByChat      *relayOrderedInts
	tombstoneSwallowLogged *relayOrderedInts
	lifecycleAckTasks      map[*adapterTask]struct{}
	draftStreamIsMessage   bool
	revocationMonitor      *relayTask
	mediaClient            *RelayMediaClient
	pendingPrompts         map[string]map[string]any
	promptOwnerNonce       string
	resolvedPrompts        *relayOrderedFloats
}

var (
	_ RelayDeliveryAdapter     = (*RelayAdapter)(nil)
	_ MessageDeleter           = (*RelayAdapter)(nil)
	_ RoutingCachePrimer       = (*RelayAdapter)(nil)
	_ StatusMetadataForTurner  = (*RelayAdapter)(nil)
	_ ExecApprovalPromptSender = (*RelayAdapter)(nil)
	_ DraftAbandoner           = (*RelayAdapter)(nil)
	_ streamIsMessageProbe     = (*RelayAdapter)(nil)
)

// Adapter for the platform config with its pre handshake descriptor and an optional transport
func NewRelayAdapter(config *PlatformConfig, descriptor CapabilityDescriptor, transport RelayTransportConn) *RelayAdapter {
	nonce := make([]byte, 3)
	rand.Read(nonce)
	a := &RelayAdapter{transport: transport, scopeByChat: map[string]string{}, dmUserByChat: map[string]string{},
		chatTypeByChat: map[string]string{}, lastInboundTSByChat: map[string]string{}, platformByChat: map[string]string{},
		profileByChat: map[string]string{}, autoThreadByChat: newRelayOrdered[[2]string](), autoThreadWaiters: map[string]*relayEvent{},
		seenInbound: newRelayOrderedSet(), discordChatLabels: newRelayOrdered[relayChatLabels](),
		discordLabelsRecorded: newRelayOrdered[relayChatLabels](), admissionLanes: map[[2]string]*adapterTask{},
		openDraftByChat: newRelayOrderedInts(), sealedDraftByChat: newRelayOrderedInts(), lifecycleAckTasks: map[*adapterTask]struct{}{},
		tombstoneSwallowLogged: newRelayOrderedInts(), pendingPrompts: map[string]map[string]any{},
		promptOwnerNonce: hex.EncodeToString(nonce), resolvedPrompts: newRelayOrderedFloats()}
	a.BasePlatformAdapter = NewBasePlatformAdapter(config, PlatformRelay, a)
	a.Traits.SplitsLongMessages = true
	a.applyDescriptor(descriptor)
	a.draftStreamIsMessage = strings.ToLower(descriptor.Platform) == "slack"
	return a
}

// Transport the session ingress relay connector check reads
func (a *RelayAdapter) RelayTransport() any { return a.transport }

// Negotiated capability profile of the primary identity
func (a *RelayAdapter) Descriptor() CapabilityDescriptor {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.descriptor
}

// The connector enforces authorization, so relay users are not default denied locally
func (a *RelayAdapter) AuthorizationIsUpstream() bool { return true }

// Length function of the primary descriptor unit
func (a *RelayAdapter) MessageLenFn() func(string) int { return relayLenFn(a.Descriptor().LenUnit) }

// Transport negotiated descriptor of the platform, nil for an unknown platform or no transport
func (a *RelayAdapter) negotiatedDescriptor(platform string) *CapabilityDescriptor {
	if platform == "" {
		return nil
	}
	ws, ok := a.transport.(interface{ DescriptorForPlatform(string) any })
	if !ok {
		return nil
	}
	if d, ok := ws.DescriptorForPlatform(platform).(CapabilityDescriptor); ok {
		return &d
	}
	return nil
}

// Underlying platform of the chat as seen inbound, else the primary platform
func (a *RelayAdapter) chatPlatform(chatID string) string {
	a.mu.Lock()
	p := a.platformByChat[chatID]
	a.mu.Unlock()
	if p != "" {
		return p
	}
	return a.Descriptor().Platform
}

// Platform a chat's shared metrics carry, the primary only when one platform is fronted
func (a *RelayAdapter) metricsPlatform(chatID string) string {
	a.mu.Lock()
	p := a.platformByChat[chatID]
	a.mu.Unlock()
	if p != "" {
		return p
	}
	fronted := map[string]bool{}
	if ws, ok := a.transport.(*WebSocketRelayTransport); ok {
		for _, id := range ws.identities {
			fronted[id[0]] = true
		}
	}
	if len(fronted) <= 1 {
		return a.Descriptor().Platform
	}
	return ""
}

// Warning policy of the logical platform, the metadata or chat platform standing in
func (a *RelayAdapter) WarningNotificationsEnabled(ctx context.Context, logicalPlatform Platform, chatID string, metadata map[string]any) bool {
	platform := string(logicalPlatform)
	if platform == "" {
		if v := metadata["_relay_logical_platform"]; pyrepr.Truthy(v) {
			platform = pyrepr.Str(v)
		} else {
			platform = a.chatPlatform(chatID)
		}
	}
	return a.BasePlatformAdapter.WarningNotificationsEnabled(ctx, Platform(platform), "", nil)
}

// Descriptor governing a chat, the negotiated per platform one else the primary
func (a *RelayAdapter) descriptorForChat(chatID string) CapabilityDescriptor {
	a.mu.Lock()
	p := a.platformByChat[chatID]
	a.mu.Unlock()
	if d := a.negotiatedDescriptor(p); d != nil {
		return *d
	}
	return a.Descriptor()
}

func (a *RelayAdapter) MaxMessageLengthForChat(chatID string) (int, error) {
	return a.descriptorForChat(chatID).MaxMessageLength, nil
}

func (a *RelayAdapter) MessageLenFnForChat(chatID string) (func(string) int, error) {
	return relayLenFn(a.descriptorForChat(chatID).LenUnit), nil
}

// Draft streaming only when the chat platform advertises draft and no Slack unfurl hints are configured
func (a *RelayAdapter) SupportsDraftStreaming(chatType string, metadata map[string]any, chatID string) bool {
	desc := a.Descriptor()
	if chatID != "" {
		desc = a.descriptorForChat(chatID)
	}
	if !(desc.SupportsDraftStreaming && containsString(desc.SupportedOps, "draft")) {
		return false
	}
	platform := desc.Platform
	if chatID != "" {
		platform = a.chatPlatform(chatID)
	}
	return len(a.slackUnfurlHints(platform)) == 0
}

// Seal semantics only for a chat whose governing descriptor is Slack
func (a *RelayAdapter) StreamIsMessageForChat(chatID string) bool {
	return strings.ToLower(a.descriptorForChat(chatID).Platform) == "slack"
}

// Native task cards only when the connector advertises the op
func (a *RelayAdapter) SupportsNativeTaskCards() bool {
	return containsString(a.Descriptor().SupportedOps, "task_card")
}

// Turn runner opt in probe of the native task card lane
func (a *RelayAdapter) NativeTaskCardsEnabled() (bool, error) {
	return a.SupportsNativeTaskCards(), nil
}

// Coordination key of one turn's stream, the per turn identity before the thread anchor
func relayDraftKey(chatID string, metadata map[string]any) string {
	turnID := pyrepr.Or(metadata["message_id"], metadata["reply_to_message_id"])
	if pyrepr.Truthy(turnID) {
		return chatID + ":turn:" + pyrepr.Str(turnID)
	}
	anchor := pyrepr.Or(pyrepr.Or(metadata["thread_ts"], metadata["thread_id"]), "")
	return chatID + ":" + pyrepr.Str(anchor)
}

// Task card key anchored on the reply target, the turn identity, the thread or root
func relayCardKey(replyTo string, metadata map[string]any) string {
	var anchor any = replyTo
	for _, k := range []string{"message_id", "reply_to_message_id", "thread_ts", "thread_id"} {
		if pyrepr.Truthy(anchor) {
			break
		}
		anchor = metadata[k]
	}
	if !pyrepr.Truthy(anchor) {
		anchor = "root"
	}
	return "turn:" + pyrepr.Str(anchor)
}

// Open draft key of the send, the single open stream of the chat for an identity less send
func (a *RelayAdapter) matchOpenDraft(chatID string, metadata map[string]any) (string, bool) {
	key := relayDraftKey(chatID, metadata)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.openDraftByChat.has(key) {
		return key, true
	}
	if pyrepr.Truthy(metadata["message_id"]) || pyrepr.Truthy(metadata["reply_to_message_id"]) {
		return "", false
	}
	var candidates []string
	for _, k := range a.openDraftByChat.keys() {
		if strings.HasPrefix(k, chatID+":") {
			candidates = append(candidates, k)
		}
	}
	if len(candidates) == 1 {
		relayAdapterLog().Info(fmt.Sprintf("relay: absorbing identity-less send into the single open stream %s (single-open-stream fallback)", candidates[0]))
		return candidates[0], true
	}
	return "", false
}

// Outbound action for the chat's platform, a connector decline logged
func (a *RelayAdapter) outbound(ctx context.Context, chatID string, action map[string]any) (map[string]any, error) {
	op := pyrepr.Str(pyrepr.Or(action["op"], "?"))
	if _, has := action["op"]; !has {
		op = "?"
	}
	a.mu.Lock()
	platform := a.platformByChat[chatID]
	a.mu.Unlock()
	result, err := a.transport.SendOutbound(ctx, action, platform)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["success"]) && IsEgressDecline(result) {
		LogDecline(op, chatID, result)
	}
	return result, nil
}

// Options of one capability gated op
type relayGateOptions struct {
	declineLevel     *slog.Level
	subject          any
	platform         string
	surfaceDeclines  bool
	surfaceAmbiguous bool
}

// Result of an advertised op, nil when unadvertised, failed or declined unless surfaced
func (a *RelayAdapter) gatedOp(ctx context.Context, chatID string, action map[string]any, o relayGateOptions) map[string]any {
	op := pyrepr.Str(action["op"])
	if a.transport == nil || !a.Descriptor().SupportsOp(op) {
		return nil
	}
	platform := o.platform
	if platform == "" {
		a.mu.Lock()
		platform = a.platformByChat[chatID]
		a.mu.Unlock()
	}
	result, err := a.transport.SendOutbound(ctx, action, platform)
	if err != nil {
		relayAdapterLog().Debug(fmt.Sprintf("relay %s transport failure", op), "exc_info", err)
		return nil
	}
	subject := o.subject
	if subject == nil {
		subject = chatID
	}
	if !pyrepr.Truthy(result["success"]) {
		if o.surfaceAmbiguous && pyrepr.Truthy(result["ambiguous"]) {
			return result
		}
		if IsEgressDecline(result) {
			LogDecline(op, subject, result)
			if o.surfaceDeclines {
				return result
			}
			return nil
		}
		if o.declineLevel != nil {
			relayAdapterLog().Log(ctx, *o.declineLevel, fmt.Sprintf("relay %s declined for %s: %s", op, pyrepr.Str(subject), pyrepr.Str(result["error"])))
		}
		return nil
	}
	return result
}
