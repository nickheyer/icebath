package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Failure of the connector enroll call carrying its user facing message
type enrollError struct{ message string }

func (e *enrollError) Error() string { return e.message }

// String value of a namespace key, empty for an absent or None value
func nsText(ns map[string]any, key string) string {
	if s, ok := ns[key].(string); ok {
		return s
	}
	return ""
}

// gw then the host name, or gw-icebath when the host name is empty
func defaultGatewayID() string {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	host = pystr.Strip(host)
	if host == "" {
		host = "icebath"
	}
	return "gw-" + host
}

// Connector base URL from the flag, GATEWAY_RELAY_URL or gateway.relay_url, ws schemes mapped to http and relay suffix dropped
func resolveConnectorURL(ctx context.Context, override string) string {
	raw := override
	if raw == "" {
		raw = os.Getenv("GATEWAY_RELAY_URL")
	}
	raw = pystr.Strip(raw)
	if raw == "" {
		section := config.EffectiveUserConfig(ctx)["gateway"]
		if !pyrepr.Truthy(section) {
			section = map[string]any{}
		}
		if block, ok := section.(map[string]any); ok && pyrepr.Truthy(block["relay_url"]) {
			raw = pystr.Strip(pyrepr.Str(block["relay_url"]))
		}
	}
	if raw == "" {
		return ""
	}
	for _, pair := range [][2]string{{"ws://", "http://"}, {"wss://", "https://"}} {
		if strings.HasPrefix(raw, pair[0]) {
			raw = pair[1] + raw[len(pair[0]):]
			break
		}
	}
	return strings.TrimSuffix(pystr.RstripChars(raw, "/"), "/relay")
}

// POSTs the enrollment token to the connector relay enroll route and returns its secret bearing payload
func postEnroll(ctx context.Context, connectorBaseURL, accessToken, enrollmentToken, gatewayID string, timeout time.Duration) (*pyjson.Object, error) {
	url := pystr.RstripChars(connectorBaseURL, "/") + "/relay/enroll"
	body := pyjson.NewObject()
	body.Set("enrollmentToken", enrollmentToken)
	body.Set("gatewayId", gatewayID)
	data, err := pyjson.Dumps(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	raw, err := gateway.RelayURLOpen(req, timeout)
	var httpErr *gateway.RelayHTTPError
	var urlErr *gateway.RelayURLError
	switch {
	case errors.As(err, &httpErr):
		detail := ""
		if text, decErr := pystr.DecodeUTF8(httpErr.Body); decErr == nil {
			if parsed, loadErr := pyjson.Loads(text); loadErr == nil {
				if !pyrepr.Truthy(parsed) {
					parsed = pyjson.NewObject()
				}
				if obj, ok := parsed.(*pyjson.Object); ok {
					if v := obj.GetOr("error", ""); pyrepr.Truthy(v) {
						detail = pyrepr.Str(v)
					}
				}
			}
		}
		var message string
		switch {
		case httpErr.Code == 401:
			message = "Connector rejected the caller identity (401). Your Nous Portal " +
				"token could not be verified — try `icebath auth add nous` and retry."
		case httpErr.Code == 403:
			message = detail
			if message == "" {
				message = "Enrollment token invalid, expired, already used, or tenant mismatch (403)."
			}
		default:
			message = fmt.Sprintf("Connector returned HTTP %d", httpErr.Code)
			if detail != "" {
				message += ": " + detail
			}
		}
		return nil, &enrollError{message}
	case errors.As(err, &urlErr):
		return nil, &enrollError{fmt.Sprintf("Could not reach the connector at %s: %s", connectorBaseURL, urlErr.Reason)}
	case err != nil:
		return nil, err
	}
	text, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return nil, err
	}
	payload, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	obj, ok := payload.(*pyjson.Object)
	if !ok || !pyrepr.Truthy(obj.GetOr("secret", nil)) {
		return nil, &enrollError{"Connector returned an unexpected response (no secret)."}
	}
	return obj, nil
}

// Prints each line and exits 1
func enrollFail(w io.Writer, lines ...string) (int, error) {
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	return 1, nil
}

// Enrolls this gateway with a relay connector and persists the relay auth credentials to the profile env file
func CmdGatewayEnroll(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		return enrollFail(w, "✗ `icebath gateway enroll` is not available in a managed/hosted install.\n"+
			"  The relay gateway secret is provisioned by the hosting platform.")
	}
	enrollmentToken := nsText(ns, "token")
	if enrollmentToken == "" {
		enrollmentToken = os.Getenv("GATEWAY_RELAY_ENROLL_TOKEN")
	}
	enrollmentToken = pystr.Strip(enrollmentToken)
	if enrollmentToken == "" {
		return enrollFail(w, "✗ No enrollment token. Pass --token <token> (or set "+
			"GATEWAY_RELAY_ENROLL_TOKEN).\n"+
			"  The connector mints this single-use token when your tenant's route "+
			"is provisioned; it is delivered with your gateway config.")
	}
	connectorBaseURL := resolveConnectorURL(ctx, nsText(ns, "connector_url"))
	if connectorBaseURL == "" {
		return enrollFail(w, "✗ No connector URL. Pass --connector-url <url> (or set GATEWAY_RELAY_URL "+
			"/ gateway.relay_url in config.yaml).")
	}
	gatewayID := nsText(ns, "gateway_id")
	if gatewayID == "" {
		gatewayID = defaultGatewayID()
	}
	gatewayID = pystr.Strip(gatewayID)
	accessToken, err := gateway.ResolveRelayIdentityToken(ctx)
	if err != nil {
		var authErr *provider.AuthError
		if errors.As(err, &authErr) {
			if authErr.ReloginRequired {
				return enrollFail(w, "✗ You're not logged into Nous Portal.",
					"  Run `icebath setup` (or `icebath auth add nous`) first, then retry.")
			}
			return enrollFail(w, fmt.Sprintf("✗ Could not resolve a Nous Portal access token: %s", err))
		}
		return enrollFail(w, fmt.Sprintf("✗ Could not resolve a caller-identity token: %s", err))
	}
	result, err := postEnroll(ctx, connectorBaseURL, accessToken, enrollmentToken, gatewayID, 15*time.Second)
	if err != nil {
		var enrollErr *enrollError
		if !errors.As(err, &enrollErr) {
			return 1, err
		}
		return enrollFail(w, fmt.Sprintf("✗ Enrollment failed: %s", enrollErr.message))
	}
	text := func(key, fallback string) string {
		if v := result.GetOr(key, nil); pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		return fallback
	}
	tenant := text("tenant", "")
	resolvedGatewayID := text("gatewayId", gatewayID)
	toWrite := [][2]string{
		{"GATEWAY_RELAY_ID", resolvedGatewayID},
		{"GATEWAY_RELAY_SECRET", text("secret", "")},
		{"GATEWAY_RELAY_DELIVERY_KEY", text("deliveryKey", "")},
	}
	anyExplicit := false
	for _, pair := range [][2]string{{"connector_url", "GATEWAY_RELAY_URL"}, {"wake_url", "GATEWAY_RELAY_WAKE_URL"}} {
		if v := pystr.Strip(nsText(ns, pair[0])); v != "" {
			anyExplicit = true
			toWrite = append(toWrite, [2]string{pair[1], pystr.RstripChars(v, "/")})
		}
	}
	for _, kv := range toWrite {
		if kv[1] == "" {
			continue
		}
		if err := config.SaveEnvValueContext(ctx, h, kv[0], kv[1]); err != nil {
			return enrollFail(w, fmt.Sprintf("✗ Failed to write %s to .env: %s", kv[0], err))
		}
	}
	line := fmt.Sprintf("✓ Enrolled gateway \"%s\"", resolvedGatewayID)
	if tenant != "" {
		line += " for tenant " + tenant
	}
	fmt.Fprintln(w, line)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Wrote to %s:\n", home.EnvPath(h))
	for _, kv := range toWrite {
		shown := kv[1]
		if kv[0] == "GATEWAY_RELAY_SECRET" || kv[0] == "GATEWAY_RELAY_DELIVERY_KEY" {
			shown = "<hidden>"
		}
		fmt.Fprintf(w, "    %s=%s\n", kv[0], shown)
	}
	fmt.Fprintln(w)
	if !(anyExplicit && warnIfSecondaryMultiplexProfile(ctx, w)) {
		fmt.Fprintln(w, "  The gateway now authenticates its relay WS upgrade with the per-gateway\n"+
			"  secret and verifies signed inbound deliveries with the tenant delivery\n"+
			"  key. Restart the gateway to pick up the new env.")
	}
	return 0, nil
}

// Warns when relay URL stamps landed in a secondary profile of a multiplexed gateway, true when it fired
func warnIfSecondaryMultiplexProfile(ctx context.Context, w io.Writer) (fired bool) {
	defer func() {
		if recover() != nil {
			fired = false
		}
	}()
	defaultRoot, err := pypath.Resolve(home.Root())
	if err != nil {
		return false
	}
	h, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return false
	}
	if _, under := pypath.PathRelativeTo(h, pypath.PathJoin(defaultRoot, "profiles")); !under {
		return false
	}
	multiplexes, err := gateway.DefaultGatewayMultiplexes(ctx, defaultRoot)
	if err != nil || !multiplexes {
		return false
	}
	fmt.Fprintln(w, "  ⚠ This profile is a SECONDARY profile of a multiplexed gateway.\n"+
		"    GATEWAY_RELAY_URL / GATEWAY_RELAY_WAKE_URL are process-level\n"+
		"    deployment settings: the gateway reads them from the process\n"+
		"    environment (or the default profile's .env), not from this\n"+
		"    profile's .env. Set them in the environment the gateway process\n"+
		"    is launched with, or enroll from the default profile. The\n"+
		"    relay credentials written above are valid either way.")
	return true
}
