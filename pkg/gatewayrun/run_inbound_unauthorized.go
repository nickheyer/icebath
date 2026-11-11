package gatewayrun

import (
	"container/list"
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	maxSenderNameChars = 64
	maxSeenSenders     = 2048
	unauthorizedSigils = "@<>[]()`*_~#"
)

// Profile flag of the approve command when the store belongs to a non default profile, else empty
func pairingProfileArg(store *gateway.PairingStore) string {
	if store == nil {
		return ""
	}
	if profile, scoped := store.Profile(); scoped && profile != "" && profile != "default" {
		return fmt.Sprintf("-p %s ", profile)
	}
	return ""
}

// Pairing DM for a first time sender with the code, its lifetime and the approve command
func pairingCodeReply(ctx context.Context, platformName, code, profileArg string) string {
	hours := max(1, gateway.PairingCodeTTLSeconds/3600)
	key := "gateway.pairing.validity_hours"
	if hours == 1 {
		key = "gateway.pairing.validity_hour"
	}
	validity := i18n.T(ctx, key, "", map[string]any{"hours": hours})
	approveCmd := fmt.Sprintf("icebath %spairing approve %s %s", profileArg, platformName, code)
	return i18n.T(ctx, "gateway.pairing.code_reply", "", map[string]any{"code": code, "validity": validity, "approve_cmd": approveCmd})
}

// Reply to a sender whose pairing requests are rate limited
func pairingRateLimitedReply(ctx context.Context) string {
	return i18n.T(ctx, "gateway.pairing.rate_limited", "", nil)
}

// Owner hint naming the dropped sender with mention and link sigils stripped and the allowlist fix
func unauthorizedOwnerHint(ctx context.Context, platformName, userID, userName, hermesHome string) string {
	safeName := gateway.NeutralizeUntrustedInlineText(userName, maxSenderNameChars)
	safeName = pystr.Strip(strings.Map(func(r rune) rune {
		if strings.ContainsRune(unauthorizedSigils, r) {
			return -1
		}
		return r
	}, safeName))
	who := userID
	if safeName != "" {
		who = fmt.Sprintf("%s (%s)", safeName, userID)
	}
	allowlist := i18n.T(ctx, "gateway.unauthorized.owner_hint_generic", "", nil)
	if envVar := gateway.AllowlistEnvForPlatform(ctx, platformName); envVar != "" {
		allowlist = i18n.T(ctx, "gateway.unauthorized.owner_hint_env", "", map[string]any{"env_var": envVar, "home": hermesHome})
	}
	return i18n.T(ctx, "gateway.unauthorized.owner_notice", "", map[string]any{
		"platform": platformName, "who": who, "allowlist": allowlist, "home": hermesHome,
	})
}

// Remembers which unrecognized senders the owner was already told about, bounded and least recent first
type UnauthorizedOwnerNotifier struct {
	seen    map[[2]string]*list.Element
	order   *list.List
	maxSeen int
}

// Notifier remembering at most maxSeen senders, at least one
func NewUnauthorizedOwnerNotifier(maxSeen int) *UnauthorizedOwnerNotifier {
	return &UnauthorizedOwnerNotifier{seen: map[[2]string]*list.Element{}, order: list.New(), maxSeen: max(1, maxSeen)}
}

// True on the first drop of a platform user, evicting the oldest remembered sender past the bound
func (n *UnauthorizedOwnerNotifier) FirstTime(platformName, userID string) bool {
	key := [2]string{platformName, userID}
	if el, ok := n.seen[key]; ok {
		n.order.MoveToBack(el)
		return false
	}
	n.seen[key] = n.order.PushBack(key)
	for len(n.seen) > n.maxSeen {
		oldest := n.order.Front()
		n.order.Remove(oldest)
		delete(n.seen, oldest.Value.([2]string))
	}
	return true
}

// Posts the hint to the home channel of the source platform unless that channel is the sender's DM
func (n *UnauthorizedOwnerNotifier) Notify(ctx context.Context, runner *GatewayRunner, source *gateway.SessionSource, hint string) error {
	for _, target := range runner.homeChannelTransports(ctx) {
		if target.platform != source.Platform {
			continue
		}
		if target.home.ChatID == source.ChatID {
			return nil
		}
		_, err := config.PresentNotification(ctx, func(ctx context.Context) error {
			runner.sendHomeChannelMessage(ctx, target.platform, target.home, target.transport,
				"⚠️ "+hint, "unauthorized-sender notice failed for %s:%s: %s")
			return nil
		}, string(target.platform), true, nil)
		return err
	}
	return nil
}

// Unauthorized DM under the ignore behaviour, the owner told in the log and once per sender in the home channel
func (r *GatewayRunner) hmReportIgnoredDM(ctx context.Context, source *gateway.SessionSource) error {
	platformName := "unknown"
	if source.Platform != "" {
		platformName = string(source.Platform)
	}
	userID, userName := "", ""
	if source.UserID != nil {
		userID = *source.UserID
	}
	if source.UserName != nil {
		userName = *source.UserName
	}
	hint := unauthorizedOwnerHint(ctx, platformName, userID, userName, home.Display(home.Dir(ctx)))
	inboundLog().Warn("Unauthorized user (ignored): " + hint)
	if r.unauthorizedOwnerNotifier == nil {
		r.unauthorizedOwnerNotifier = NewUnauthorizedOwnerNotifier(maxSeenSenders)
	}
	if r.unauthorizedOwnerNotifier.FirstTime(platformName, userID) && r.config != nil {
		return r.unauthorizedOwnerNotifier.Notify(ctx, r, source, hint)
	}
	return nil
}
