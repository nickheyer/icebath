package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	sideQuestionCapabilityID     = "llm.side_question"
	sideQuestionLeaseTTLSeconds  = 60.0
	sideQuestionMaxPromptChars   = 4096
	sideQuestionDefaultTimeout   = 15.0
	sideQuestionMaxTimeout       = 60.0
	sideQuestionTokenEntropySize = 24
)

// Captured turn behind one token, its expiry and the profile home key it ran under
type sideQuestionLease struct {
	capture   *sideQuestionCapture
	expiresAt float64
	homeKey   string
}

var (
	sideQuestionLeases  = map[string]*sideQuestionLease{}
	sideQuestionLeaseMu sync.Mutex
	sideQuestionNow     = pythread.Monotonic
	askSideQuestion     = askTextOnly
)

func sideQuestionLeaseLog() *slog.Logger {
	return slog.Default().With("logger", "agent.side_question_lease")
}

// Profile config the capability grant is read from, swapped by tests
var sideQuestionGrantConfig = func(ctx context.Context) (map[string]any, error) {
	h := home.Dir(ctx)
	return config.Load(h, config.NewSecrets(h).Lookup())
}

// True when a loaded enabled plugin declares llm.side_question and the user granted it
func consentedPluginLoaded(ctx context.Context) bool {
	keys := plugins.GetPluginManager(ctx).EnabledPluginsDeclaring(sideQuestionCapabilityID)
	if len(keys) == 0 {
		return false
	}
	cfg, err := sideQuestionGrantConfig(ctx)
	if err != nil {
		return false
	}
	for _, key := range keys {
		if plugins.CapabilityGranted(cfg, key, sideQuestionCapabilityID) {
			return true
		}
	}
	return false
}

// Drops expired and orphaned leases and, on a new turn, the agent's older ones, lock held
func purgeSideQuestionLeasesLocked(now float64, a *AIAgent) {
	for token, lease := range sideQuestionLeases {
		parent := lease.capture.parent.Value()
		if lease.expiresAt < now || parent == nil || (a != nil && parent == a) {
			delete(sideQuestionLeases, token)
		}
	}
}

// A turn nobody waits on, a persistence detached fork, a cron run or a delegated child
func isUnattendedTurn(a *AIAgent) bool {
	return a.persistDisabled || a.platform == "cron" || a.delegateDepth > 0
}

// Leases the finished turn under a fresh token, nil for an unattended turn or no consented plugin
func issueSideQuestionToken(ctx context.Context, a *AIAgent, messages []map[string]any, turnID string) (any, error) {
	if isUnattendedTurn(a) || !consentedPluginLoaded(ctx) {
		return nil, nil
	}
	toolsCopy := pycopy.Of(a.tools)
	if toolsCopy == nil {
		toolsCopy = []any{}
	}
	captured := context.WithoutCancel(ctx)
	capture := &sideQuestionCapture{
		parent: weak.Make(a), messages: pycopy.Of(messages), systemPrompt: a.cachedSystemPrompt,
		systemPromptStatic: a.cachedSystemPromptStatic, tools: toolsCopy, sessionID: a.sessionID, turnID: turnID,
		runtime: sideQuestionRuntimeKey(a), ctx: pyctx.With(captured, pyctx.CopyContext(ctx)),
	}
	raw := make([]byte, sideQuestionTokenEntropySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := sideQuestionNow()
	homeKey := home.Key(home.Dir(ctx))
	sideQuestionLeaseMu.Lock()
	defer sideQuestionLeaseMu.Unlock()
	purgeSideQuestionLeasesLocked(now, a)
	sideQuestionLeases[token] = &sideQuestionLease{capture: capture, expiresAt: now + sideQuestionLeaseTTLSeconds, homeKey: homeKey}
	return token, nil
}

// Why the popped lease cannot be redeemed with the prompt, empty when it can
func sideQuestionRedeemable(lease *sideQuestionLease, prompt any) string {
	if lease == nil {
		return "unknown_token"
	}
	if sideQuestionNow() > lease.expiresAt {
		return "expired"
	}
	parent := lease.capture.parent.Value()
	if parent == nil {
		return "parent_gone"
	}
	if parent.currentTurnID != lease.capture.turnID {
		return "turn_moved_on"
	}
	text, ok := prompt.(string)
	if !ok || pystr.Strip(text) == "" || pyLen(text) > sideQuestionMaxPromptChars {
		return "bad_prompt"
	}
	return ""
}

// RedeemSideQuestion spends the lease behind the token on one text only question, the answer or nil
func RedeemSideQuestion(ctx context.Context, token, prompt any, pluginID, h string, timeout any) (*string, error) {
	if h == "" {
		h = home.Dir(ctx)
	}
	homeKey := home.Key(h)
	sideQuestionLeaseMu.Lock()
	key, isStr := token.(string)
	var lease *sideQuestionLease
	if isStr {
		lease = sideQuestionLeases[key]
	}
	if lease != nil && lease.homeKey != homeKey {
		sideQuestionLeaseMu.Unlock()
		sideQuestionLeaseLog().Info(fmt.Sprintf("side_question plugin=%s outcome=rejected:other_profile", pluginID))
		return nil, nil
	}
	if lease != nil {
		delete(sideQuestionLeases, key)
	}
	purgeSideQuestionLeasesLocked(sideQuestionNow(), nil)
	sideQuestionLeaseMu.Unlock()
	if reason := sideQuestionRedeemable(lease, prompt); reason != "" {
		sideQuestionLeaseLog().Info(fmt.Sprintf("side_question plugin=%s outcome=rejected:%s", pluginID, reason))
		return nil, nil
	}
	seconds := plugins.ArgFloat(timeout, sideQuestionDefaultTimeout)
	if seconds > 0 {
		seconds = min(seconds, sideQuestionMaxTimeout)
	} else {
		seconds = sideQuestionDefaultTimeout
	}
	return askSideQuestion(sideQuestionRunContext(lease.capture), lease.capture, prompt.(string), pluginID, seconds), nil
}
