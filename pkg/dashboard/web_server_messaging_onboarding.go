package dashboard

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Timer of a pairing expiry, stopped once its reservation is released
type whatsappTimer interface{ Stop() bool }

// One WhatsApp QR pairing, its bridge process, reservation and linked account, None fields held as nil
type whatsappOnboardingSession struct {
	proc            whatsappPairingProc
	mode            string
	allowedUsers    string
	sessionPath     string
	expiresAt       string
	expiresAtTS     float64
	profile         string
	status          string
	qrPayload       any
	accountID       any
	accountName     any
	accountPhone    any
	err             any
	startingProcess bool
	sessionReserved bool
	expiryTimer     whatsappTimer
}

// Pairing sessions by id in insertion order
type whatsappSessionTable struct {
	ids  []string
	byID map[string]*whatsappOnboardingSession
}

// Session of an id, nil when absent
func (t *whatsappSessionTable) get(id string) *whatsappOnboardingSession { return t.byID[id] }

// Stores a session under id, a new id going last
func (t *whatsappSessionTable) set(id string, record *whatsappOnboardingSession) {
	if _, held := t.byID[id]; !held {
		t.ids = append(t.ids, id)
	}
	t.byID[id] = record
}

// Drops the session of an id when present
func (t *whatsappSessionTable) pop(id string) {
	if _, held := t.byID[id]; !held {
		return
	}
	delete(t.byID, id)
	for i, held := range t.ids {
		if held == id {
			t.ids = append(t.ids[:i:i], t.ids[i+1:]...)
			break
		}
	}
}

// Ids in insertion order, a copy safe to range while popping
func (t *whatsappSessionTable) order() []string { return append([]string(nil), t.ids...) }

var whatsappOnboardingSessions = &whatsappSessionTable{byID: map[string]*whatsappOnboardingSession{}}

// Session directory of the WhatsApp bridge in the scoped home, the module attribute hermes tests patch
var whatsappSessionPath = func(ctx context.Context) string {
	return home.HermesDir(home.Dir(ctx), "platforms/whatsapp/session", "whatsapp/session")
}

// Status payload of a pairing with its id first
func whatsappOnboardingPayload(pairingID string, record *whatsappOnboardingSession) *pyjson.Object {
	return pyjson.ObjectOf(
		"pairing_id", pairingID,
		"status", record.status,
		"qr_payload", record.qrPayload,
		"expires_at", record.expiresAt,
		"mode", record.mode,
		"allowed_users", record.allowedUsers,
		"account_id", record.accountID,
		"account_name", record.accountName,
		"account_phone", record.accountPhone,
		"error", record.err,
	)
}

// Gateway restart after WhatsApp onboarding, the module attribute hermes tests patch
var restartGatewayAfterWhatsappOnboarding = func(ctx context.Context, profile string) (*pyjson.Object, error) {
	return restartGatewayAfter(ctx, profile, "WhatsApp onboarding", "WhatsApp onboarding")
}

const telegramOnboardingDefaultURL = "https://setup.hermes-agent.nousresearch.com"

// One Telegram QR pairing, its poll token and the bot the setup service created
type telegramOnboardingPairing struct {
	pollToken   string
	expiresAt   string
	expiresAtTS float64
	botToken    string
	botUsername any
	ownerUserID any
}

var (
	telegramOnboardingPairings = map[string]*telegramOnboardingPairing{}
	telegramOnboardingMu       sync.Mutex
)

// Setup service base URL from TELEGRAM_ONBOARDING_URL, stripped of trailing slashes
func telegramOnboardingBaseURL() string {
	raw, ok := os.LookupEnv("TELEGRAM_ONBOARDING_URL")
	if !ok {
		raw = telegramOnboardingDefaultURL
	}
	return strings.TrimRight(pystr.Strip(raw), "/")
}

var telegramOnboardingErrors = map[string]string{
	"not_found":    "Telegram pairing was not found. Start a new setup.",
	"expired":      "Telegram setup expired. Start a new setup.",
	"claimed":      "Telegram setup was already claimed. Start a new setup.",
	"unauthorized": "Telegram setup service rejected this request.",
	"telegram_manager_bot_token_not_configured": "Telegram setup service is not configured.",
	"telegram_token_fetch_failed":               "Telegram could not finish bot setup. Try again.",
}

// User facing text of a setup service error code, fallback for an unknown code
func telegramOnboardingErrorMessage(code, fallback string) string {
	if text, ok := telegramOnboardingErrors[code]; ok {
		return text
	}
	return fallback
}

const (
	telegramUnavailable = "Telegram setup service is unavailable. Try again shortly."
	telegramInvalid     = "Telegram setup service returned an invalid response."
)

// Setup service request, the module attribute hermes tests patch
var telegramOnboardingRequestSync = telegramOnboardingHTTPRequest

// JSON object the setup service answers, its error codes mapped onto 404, 410 or 502
func telegramOnboardingHTTPRequest(ctx context.Context, method, path string, body *pyjson.Object, bearerToken string) (*pyjson.Object, error) {
	agent, err := updaterecord.UserAgent("IcebathDashboard")
	if err != nil {
		return nil, err
	}
	resp, err := func() (*pyhttpx.Response, error) {
		var payload io.Reader
		if body != nil {
			text, err := pyjson.Dumps(body, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
			if err != nil {
				return nil, err
			}
			payload = bytes.NewReader([]byte(text))
		}
		req, err := http.NewRequestWithContext(ctx, method, telegramOnboardingBaseURL()+path, payload)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", agent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+bearerToken)
		}
		return pyhttpx.Do(pyhttpx.PlainClient(false), req, 10*time.Second)
	}()
	if err != nil {
		return nil, NewHTTPException(502, telegramUnavailable)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		parsed := pyjson.NewObject()
		if decoded, err := resp.JSON(); err == nil {
			obj, ok := decoded.(*pyjson.Object)
			if !ok {
				return nil, attributeGetError(decoded)
			}
			parsed = obj
		}
		code := parsed.GetOr("error", nil)
		if !pyrepr.Truthy(code) {
			code = parsed.GetOr("status", nil)
		}
		if !pyrepr.Truthy(code) {
			code = ""
		}
		text := pyrepr.Str(code)
		detail := telegramOnboardingErrorMessage(text, "Telegram setup service returned an error.")
		status := 502
		switch {
		case text == "expired" || text == "claimed":
			status = 410
		case resp.StatusCode == 404:
			status = 404
		}
		return nil, NewHTTPException(status, detail)
	}
	decoded, err := resp.JSON()
	if err != nil {
		return nil, NewHTTPException(502, telegramInvalid)
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, NewHTTPException(502, telegramInvalid)
	}
	return obj, nil
}

// AttributeError a get call on a decoded JSON value that is no dict raises
func attributeGetError(v any) error {
	return &pyAttributeError{msg: "'" + pyrepr.TypeName(v) + "' object has no attribute 'get'"}
}

// Python AttributeError with its message
type pyAttributeError struct{ msg string }

func (e *pyAttributeError) Error() string      { return e.msg }
func (e *pyAttributeError) PyTypeName() string { return "AttributeError" }
