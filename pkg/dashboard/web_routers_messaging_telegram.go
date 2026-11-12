package dashboard

import (
	"context"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	telegramSessionNotFound      = "Telegram setup session was not found. Start a new setup."
	telegramIncompleteResponse   = "Telegram setup service returned an incomplete response."
	telegramDefaultOnboardingBot = "Hermes Agent"
)

// Epoch seconds of an ISO expiry, naive read as UTC, ten minutes from now when unparseable
func parseExpiryTS(value string) float64 {
	parsed, _, err := pydatetime.ParseISOFormat(value)
	if err != nil {
		return epochNow() + 600
	}
	return pydatetime.Timestamp(parsed)
}

// Drops expired Telegram pairings, called under the onboarding lock
func pruneTelegramOnboardingPairings() {
	now := epochNow()
	for pairingID, record := range telegramOnboardingPairings {
		if record.expiresAtTS <= now {
			delete(telegramOnboardingPairings, pairingID)
		}
	}
}

// Stripped numeric Telegram user id, nil for anything else
func normalizeTelegramUserID(value any) any {
	normalized := pystr.Strip(truthyStr(value))
	if normalized == "" {
		return nil
	}
	for _, r := range normalized {
		if !unicode.Is(unicode.Nd, r) {
			return nil
		}
	}
	return normalized
}

// Telegram pairing of an id after pruning, 404 when unknown, called under the onboarding lock
func telegramRecordOr404(pairingID string) (*telegramOnboardingPairing, error) {
	pruneTelegramOnboardingPairings()
	record := telegramOnboardingPairings[pairingID]
	if record == nil {
		return nil, NewHTTPException(404, telegramSessionNotFound)
	}
	return record, nil
}

// Ready payload of a pairing whose bot exists
func telegramReadyPayload(record *telegramOnboardingPairing) *pyjson.Object {
	return pyjson.ObjectOf("status", "ready", "bot_username", record.botUsername,
		"owner_user_id", record.ownerUserID, "expires_at", record.expiresAt)
}

// Setup service request of a route, run on the request goroutine the way asyncio.to_thread runs it off the loop
func telegramOnboardingRequest(ctx context.Context, method, path string, body *pyjson.Object, bearerToken string) (*pyjson.Object, error) {
	return telegramOnboardingRequestSync(ctx, method, path, body, bearerToken)
}

// Opens a pairing with the setup service and returns its deep link and QR payload
func startTelegramOnboarding(ctx context.Context, body *pyjson.Object) (*pyjson.Object, error) {
	botName := pystr.Strip(truthyStr(body.GetOr("bot_name", nil)))
	if botName == "" {
		botName = telegramDefaultOnboardingBot
	}
	payload, err := telegramOnboardingRequest(ctx, "POST", "/v1/telegram/pairings", pyjson.ObjectOf("bot_name", botName), "")
	if err != nil {
		return nil, err
	}
	field := func(key string) string { return pystr.Strip(truthyStr(payload.GetOr(key, nil))) }
	pairingID, pollToken, expiresAt, deepLink := field("pairing_id"), field("poll_token"), field("expires_at"), field("deep_link")
	if pairingID == "" || pollToken == "" || expiresAt == "" || deepLink == "" {
		return nil, NewHTTPException(502, telegramIncompleteResponse)
	}
	telegramOnboardingMu.Lock()
	pruneTelegramOnboardingPairings()
	telegramOnboardingPairings[pairingID] = &telegramOnboardingPairing{pollToken: pollToken, expiresAt: expiresAt,
		expiresAtTS: parseExpiryTS(expiresAt)}
	telegramOnboardingMu.Unlock()
	qr := payload.GetOr("qr_payload", nil)
	if !pyrepr.Truthy(qr) {
		qr = deepLink
	}
	return pyjson.ObjectOf("pairing_id", pairingID, "suggested_username", field("suggested_username"), "deep_link", deepLink,
		"qr_payload", pystr.Strip(pyrepr.Str(qr)), "expires_at", expiresAt), nil
}

// Polls the setup service for a pairing, recording the bot token once the bot is ready
func getTelegramOnboardingStatus(ctx context.Context, pairingID string) (*pyjson.Object, error) {
	telegramOnboardingMu.Lock()
	record, err := telegramRecordOr404(pairingID)
	if err != nil {
		telegramOnboardingMu.Unlock()
		return nil, err
	}
	if record.botToken != "" {
		defer telegramOnboardingMu.Unlock()
		return telegramReadyPayload(record), nil
	}
	pollToken := record.pollToken
	telegramOnboardingMu.Unlock()
	payload, err := telegramOnboardingRequest(ctx, "GET", "/v1/telegram/pairings/"+pyurl.Quote(pairingID, ""), nil, pollToken)
	if err != nil {
		return nil, err
	}
	status := pystr.Strip(truthyStr(payload.GetOr("status", nil)))
	switch status {
	case "waiting":
		telegramOnboardingMu.Lock()
		expiresAt := ""
		if current := telegramOnboardingPairings[pairingID]; current != nil {
			expiresAt = current.expiresAt
		}
		telegramOnboardingMu.Unlock()
		return pyjson.ObjectOf("status", "waiting", "expires_at", expiresAt), nil
	case "ready":
		botToken := pystr.Strip(truthyStr(payload.GetOr("token", nil)))
		if botToken == "" {
			return nil, NewHTTPException(502, telegramIncompleteResponse)
		}
		telegramOnboardingMu.Lock()
		defer telegramOnboardingMu.Unlock()
		record := telegramOnboardingPairings[pairingID]
		if record == nil {
			return nil, NewHTTPException(404, telegramSessionNotFound)
		}
		record.botToken = botToken
		record.botUsername = noneIfEmpty(pystr.Strip(truthyStr(payload.GetOr("bot_username", nil))))
		record.ownerUserID = normalizeTelegramUserID(payload.GetOr("owner_user_id", nil))
		return telegramReadyPayload(record), nil
	case "expired", "claimed":
		telegramOnboardingMu.Lock()
		delete(telegramOnboardingPairings, pairingID)
		telegramOnboardingMu.Unlock()
		return nil, NewHTTPException(410, telegramOnboardingErrorMessage(status, "Telegram setup is no longer available. Start a new setup."))
	}
	return nil, NewHTTPException(502, "Telegram setup service returned an unknown status.")
}

// Saves the ready bot token and allowlist into the profile, then restarts the gateway
func applyTelegramOnboarding(ctx context.Context, pairingID string, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	var allowedUserIDs []string
	seen := map[string]bool{}
	rawIDs, _ := body.GetOr("allowed_user_ids", []any{}).([]any)
	normalized := make([]any, 0, len(rawIDs))
	for _, raw := range rawIDs {
		normalized = append(normalized, normalizeTelegramUserID(raw))
	}
	for _, id := range normalized {
		if id == nil {
			return nil, NewHTTPException(400, "Allowed Telegram user IDs must be numeric.")
		}
	}
	for _, id := range normalized {
		if s := id.(string); !seen[s] {
			seen[s] = true
			allowedUserIDs = append(allowedUserIDs, s)
		}
	}
	if len(allowedUserIDs) == 0 {
		return nil, NewHTTPException(400, "Add at least one allowed Telegram user ID.")
	}
	telegramOnboardingMu.Lock()
	record, err := telegramRecordOr404(pairingID)
	var botToken string
	var botUsername any
	if err == nil {
		botToken, botUsername = record.botToken, record.botUsername
		if botToken == "" {
			err = NewHTTPException(409, "Telegram setup is not ready yet.")
		}
	}
	telegramOnboardingMu.Unlock()
	if err != nil {
		return nil, err
	}
	effectiveProfile := firstProfile(body.GetOr("profile", nil), profile)
	_, err = scopedToThread(ctx, effectiveProfile, func(sctx context.Context) (struct{}, error) {
		h := home.Dir(sctx)
		if err := config.SaveEnvValueContext(sctx, h, "TELEGRAM_BOT_TOKEN", botToken); err != nil {
			return struct{}{}, err
		}
		if err := config.SaveEnvValueContext(sctx, h, "TELEGRAM_ALLOWED_USERS", strings.Join(allowedUserIDs, ",")); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, writePlatformEnabled(sctx, "telegram", true)
	})
	if err := onboardingSaveErrors(err, "Telegram onboarding apply failed", "Failed to save Telegram setup."); err != nil {
		return nil, err
	}
	telegramOnboardingMu.Lock()
	delete(telegramOnboardingPairings, pairingID)
	telegramOnboardingMu.Unlock()
	restartResult, err := restartGatewayAfter(ctx, effectiveProfile, "Telegram onboarding", "Telegram onboarding")
	if err != nil {
		return nil, err
	}
	out := pyjson.ObjectOf("ok", true, "platform", "telegram", "bot_username", botUsername,
		"needs_restart", !pyrepr.Truthy(restartResult.GetOr("restart_started", nil)))
	for _, k := range restartResult.Keys {
		out.Set(k, restartResult.Vals[k])
	}
	return out, nil
}

// Forgets a Telegram pairing
func cancelTelegramOnboarding(pairingID string) *pyjson.Object {
	telegramOnboardingMu.Lock()
	delete(telegramOnboardingPairings, pairingID)
	telegramOnboardingMu.Unlock()
	return pyjson.ObjectOf("ok", true)
}
