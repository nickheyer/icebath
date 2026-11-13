package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyqrcode"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Nous hosted Telegram pairing API and the onboarding defaults of a managed bot
const (
	TelegramOnboardingDefaultURL = "https://setup.hermes-agent.nousresearch.com"
	TelegramOnboardingURLEnv     = "TELEGRAM_ONBOARDING_URL"
	telegramDefaultBotName       = "Icebath Agent"
	telegramDefaultPollTimeout   = 180.0
	telegramPollInterval         = 2.0
)

// Pairing record the onboarding service returns, an empty ExpiresAt meaning none
type TelegramPairing struct {
	PairingID, PollToken, SuggestedUsername, DeepLink, QRPayload string
	ExpiresAt                                                    *string
}

// Ready onboarding result, the bot username and owner user id empty when the service sent none
type TelegramBotSetupResult struct {
	Token, BotUsername, OwnerUserID string
}

// HTTP, clock, sleep, output and QR seams of the onboarding client, the module names its tests patch
var (
	telegramHTTPPost = func(ctx context.Context, url string, body []byte, timeout time.Duration) (*pyhttpx.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return telegramDo(req, timeout)
	}
	telegramHTTPGet = func(ctx context.Context, url string, headers map[string]string, timeout time.Duration) (*pyhttpx.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return telegramDo(req, timeout)
	}
	telegramMonotonic = func() float64 { return float64(time.Since(telegramClockBase)) / float64(time.Second) }
	telegramSleep     = func(seconds float64) { time.Sleep(time.Duration(seconds * float64(time.Second))) }
	telegramOut       = provider.SetupOutput
	telegramRenderQR  = renderQRTerminal
	telegramClockBase = time.Now()
)

// Managed QR bot creation of the setup flows, the auto_setup_telegram_bot_result name tests replace
var telegramAutoSetupFn = func(ctx context.Context, profileName *string) (*TelegramBotSetupResult, error) {
	return autoSetupTelegramBotResult(ctx, "", profileName, telegramDefaultPollTimeout)
}

// Transport failure of the onboarding client, the httpx.HTTPError class
type telegramHTTPError struct{ err error }

func (e *telegramHTTPError) Error() string { return e.err.Error() }
func (e *telegramHTTPError) Unwrap() error { return e.err }

// Undecodable response body, the ValueError class json.loads raises
type telegramValueError struct{ err error }

func (e *telegramValueError) Error() string { return e.err.Error() }
func (e *telegramValueError) Unwrap() error { return e.err }

// Sends a request with httpx timeouts, a transport failure raised as the httpx error class
func telegramDo(req *http.Request, timeout time.Duration) (*pyhttpx.Response, error) {
	resp, err := pyhttpx.Do(pyhttpx.PlainClient(false), req, timeout)
	if err != nil {
		return nil, &telegramHTTPError{err}
	}
	return resp, nil
}

// Whether a failure is the httpx error or ValueError class the onboarding client swallows
func telegramSwallowed(err error) bool {
	var transport *telegramHTTPError
	var value *telegramValueError
	return errors.As(err, &transport) || errors.As(err, &value)
}

// Onboarding API URL, the explicit one, the env override or the default, without a trailing slash
func telegramAPIURL(apiURL string) string {
	if apiURL == "" {
		apiURL = os.Getenv(TelegramOnboardingURLEnv)
	}
	if apiURL == "" {
		apiURL = TelegramOnboardingDefaultURL
	}
	return strings.TrimRight(apiURL, "/")
}

// Whether a decoded value is a string with the Telegram bot token shape
func isValidTelegramBotTokenValue(token any) bool {
	s, ok := token.(string)
	return ok && isValidTelegramBotToken(s)
}

// Positive owner user id of a decoded value as decimal text, empty for anything else
func parseOwnerUserID(value any) string {
	if _, isBool := value.(bool); isBool {
		return ""
	}
	if s, ok := value.(string); ok && s != "" && pystr.All(s, pystr.IsDecimal) {
		n, _ := pyint.Parse(s)
		value = n
	}
	var n *big.Int
	switch v := value.(type) {
	case int64:
		n = big.NewInt(v)
	case *big.Int:
		n = v
	default:
		return ""
	}
	if n.Sign() <= 0 {
		return ""
	}
	return n.String()
}

// Terminal QR code of a URL as qrcode prints it inverted with a one module border
func renderQRTerminal(url string) (string, error) {
	code, err := pyqrcode.Make(url, pyqrcode.ErrorCorrectL, 1)
	if err != nil {
		return "", err
	}
	return code.PrintASCII(true), nil
}

// Prints a QR code of the URL and the link when asked
func printQRCode(url string, includeLink bool) error {
	w := telegramOut()
	rendered, err := telegramRenderQR(url)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, rendered)
	if includeLink {
		fmt.Fprintf(w, "  Link: %s\n", url)
	}
	return nil
}

// Decoded JSON body of a response, any decode failure raised as the ValueError class
func telegramResponseJSON(resp *pyhttpx.Response) (any, error) {
	data, err := resp.JSON()
	if err != nil {
		return nil, &telegramValueError{err}
	}
	return data, nil
}

// JSON object of a decoded body, the AttributeError dict.get raises on anything else
func telegramObject(data any) (*pyjson.Object, error) {
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(data))
	}
	return obj, nil
}

// Posts a pairing request, nil on a transport failure, a bad status or an incomplete record
func createTelegramPairing(ctx context.Context, apiURL, botName string, timeout time.Duration) (*TelegramPairing, error) {
	body, err := pyjson.EncodeWTF8(pyjson.ObjectOf("bot_name", botName))
	if err != nil {
		return nil, err
	}
	resp, err := telegramHTTPPost(ctx, telegramAPIURL(apiURL)+"/v1/telegram/pairings", body, timeout)
	var decoded any
	if err == nil && (resp.StatusCode == 200 || resp.StatusCode == 201) {
		decoded, err = telegramResponseJSON(resp)
	} else if err == nil {
		return nil, nil
	}
	if err != nil {
		if telegramSwallowed(err) {
			return nil, nil
		}
		return nil, err
	}
	data, err := telegramObject(decoded)
	if err != nil {
		return nil, err
	}
	fields := make([]string, 0, 4)
	for _, key := range []string{"pairing_id", "poll_token", "suggested_username", "deep_link"} {
		s, ok := data.Vals[key].(string)
		if !ok || s == "" {
			return nil, nil
		}
		fields = append(fields, s)
	}
	qrPayload := data.Vals["qr_payload"]
	if !pyrepr.Truthy(qrPayload) {
		qrPayload = fields[3]
	}
	qr, ok := qrPayload.(string)
	if !ok {
		return nil, nil
	}
	pairing := &TelegramPairing{PairingID: fields[0], PollToken: fields[1], SuggestedUsername: fields[2], DeepLink: fields[3], QRPayload: qr}
	if expires, ok := data.Vals["expires_at"].(string); ok {
		pairing.ExpiresAt = &expires
	}
	return pairing, nil
}

// Polls the onboarding service once, the setup result when the bot is ready
func pollTelegramPairingResultOnce(ctx context.Context, apiURL string, pairing *TelegramPairing, timeout time.Duration) (*TelegramBotSetupResult, error) {
	resp, err := telegramHTTPGet(ctx, telegramAPIURL(apiURL)+"/v1/telegram/pairings/"+pairing.PairingID,
		map[string]string{"Authorization": "Bearer " + pairing.PollToken}, timeout)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, nil
	}
	decoded, err := telegramResponseJSON(resp)
	if err != nil {
		return nil, err
	}
	data, err := telegramObject(decoded)
	if err != nil {
		return nil, err
	}
	token := data.Vals["token"]
	if status, _ := data.Vals["status"].(string); status != "ready" || !isValidTelegramBotTokenValue(token) {
		return nil, nil
	}
	result := &TelegramBotSetupResult{Token: token.(string), OwnerUserID: parseOwnerUserID(data.Vals["owner_user_id"])}
	if username, ok := data.Vals["bot_username"].(string); ok {
		result.BotUsername = username
	}
	return result, nil
}

// Polls until the setup result arrives or the timeout passes, transport and JSON failures counting as not ready
func pollForTelegramSetupResult(ctx context.Context, apiURL string, pairing *TelegramPairing, timeout, interval float64, onTick func(elapsed float64)) (*TelegramBotSetupResult, error) {
	start := telegramMonotonic()
	deadline := start + timeout
	for telegramMonotonic() < deadline {
		if onTick != nil {
			onTick(telegramMonotonic() - start)
		}
		result, err := pollTelegramPairingResultOnce(ctx, apiURL, pairing, 10*time.Second)
		switch {
		case err == nil && result != nil:
			return result, nil
		case err != nil && !telegramSwallowed(err):
			return nil, err
		}
		telegramSleep(interval)
	}
	return nil, nil
}

// Runs the automatic Telegram bot creation, profileName accepted for callers because the service names the bot
func autoSetupTelegramBotResult(ctx context.Context, apiURL string, profileName *string, pollTimeout float64) (*TelegramBotSetupResult, error) {
	_ = profileName
	w := telegramOut()
	resolved := telegramAPIURL(apiURL)
	fmt.Fprintf(w, "\n  Contacting Icebath Telegram onboarding service: %s\n", resolved)
	pairing, err := createTelegramPairing(ctx, resolved, telegramDefaultBotName, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if pairing == nil {
		fmt.Fprintln(w, "  ✗ Could not reach the Icebath Telegram onboarding service.\n"+
			"    Try the manual setup instead, or check your network.")
		return nil, nil
	}
	fmt.Fprintln(w, "  ✓ Pairing created\n  Rendering QR code...")
	fmt.Fprint(w, "\n  Scan this QR code with your phone, or open the link below:\n\n")
	if err := printQRCode(pairing.QRPayload, false); err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "\n  Link: %s\n"+
		"  When Telegram opens, tap 'Create Bot' to confirm.\n"+
		"  (You can edit the bot display name before confirming)\n\n", pairing.DeepLink)
	spinner := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	tick := 0
	spin := func(elapsed float64) {
		char := spinner[tick%len(spinner)]
		tick++
		remaining := max(0, int(pollTimeout-float64(int(elapsed))))
		io.WriteString(w, fmt.Sprintf("\r  %c Waiting for bot creation... (%ds remaining) ", char, remaining))
	}
	result, err := pollForTelegramSetupResult(ctx, resolved, pairing, pollTimeout, telegramPollInterval, spin)
	if err != nil {
		return nil, err
	}
	if result != nil {
		io.WriteString(w, "\r  ✓ Bot created successfully!                              \n")
		return result, nil
	}
	io.WriteString(w, "\r  ✗ Timed out waiting for bot creation.                    \n")
	fmt.Fprintln(w, "    The bot may still be created — check Telegram.\n"+
		"    You can paste the token manually below, or re-run setup.")
	return nil, nil
}
