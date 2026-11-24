package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Delays in seconds before each boot provision retry, the last one repeating
var relayProvisionRetryDelays = []float64{5.0, 30.0, 120.0, 300.0}

// Seconds between retries after the connector refused with a reason
var relayProvisionRefusalRetry = 600.0

const (
	relayProvisioned = "provisioned"
	relaySkipped     = "skipped"
	relayRefused     = "refused"
	relayFailed      = "failed"
)

// RelayProvisionRefused is a connector refusal of provision carrying its own reason
type RelayProvisionRefused struct{ Msg string }

func (e *RelayProvisionRefused) Error() string { return e.Msg }

// RelayRuntimeError is a provision or policy failure with a user facing message
type RelayRuntimeError struct{ Msg string }

func (e *RelayRuntimeError) Error() string { return e.Msg }

// Bearer authenticated JSON POST returning the open 2xx response
func relayJSONPost(url, token string, body *jsonutil.Object, timeout time.Duration) (*http.Response, error) {
	data, err := jsonutil.Encode(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return relayURLOpenResponse(req, timeout)
}

// Arguments of one provision POST
type relayProvisionArgs struct {
	provisionURL    string
	accessToken     string
	gatewayID       string
	platform        string
	botID           string
	gatewayEndpoint string
	routeKeys       []string
	instanceID      string
	wakeURL         string
	displayName     string
	timeout         time.Duration
}

// Provision response object, RelayProvisionRefused for a reasoned refusal and RelayRuntimeError for other failures
func relayPostProvision(a relayProvisionArgs) (*jsonutil.Object, error) {
	routeKeys := make([]any, len(a.routeKeys))
	for i, k := range a.routeKeys {
		routeKeys[i] = k
	}
	body := jsonutil.ObjectOf("gatewayId", a.gatewayID, "platform", a.platform, "botId", a.botID,
		"gatewayEndpoint", a.gatewayEndpoint, "routeKeys", routeKeys)
	for _, kv := range [][2]string{{"instanceId", a.instanceID}, {"wakeUrl", a.wakeURL}, {"displayName", a.displayName}} {
		if kv[1] != "" {
			body.Set(kv[0], kv[1])
		}
	}
	timeout := a.timeout
	if timeout == 0 {
		timeout = relayHTTPTimeout
	}
	resp, err := relayJSONPost(a.provisionURL, a.accessToken, body, timeout)
	if err != nil {
		var httpErr *RelayHTTPError
		if errors.As(err, &httpErr) {
			errBody := RelayReadErrorBody(httpErr)
			detail := RelayRedactExcerpt(RelayConnectorReason(errBody), a.accessToken)
			if detail != "" && httpErr.Code >= 400 && httpErr.Code < 500 && httpErr.Code != 408 && httpErr.Code != 429 && !RelayAnsweredByProxy(httpErr) {
				return nil, &RelayProvisionRefused{Msg: fmt.Sprintf("connector returned HTTP %d: %s", httpErr.Code, detail)}
			}
			described := RelayDescribeHTTPError(httpErr, errBody, a.accessToken)
			if detail != "" {
				described += ": " + detail
			}
			return nil, &RelayRuntimeError{Msg: described}
		}
		var urlErr *RelayURLError
		if errors.As(err, &urlErr) {
			return nil, &RelayRuntimeError{Msg: "could not reach connector: " + urlErr.Reason}
		}
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	text, err := textutil.DecodeUTF8(raw)
	if err != nil {
		return nil, err
	}
	parsed, err := jsonutil.Decode(text)
	if err != nil {
		return nil, err
	}
	payload, ok := jsonutil.AsObject(parsed)
	if !ok {
		return nil, &RelayRuntimeError{Msg: "connector returned an unexpected response"}
	}
	if issued, has := payload.Get("secretIssued"); !anyutil.IsTrue(payload.ValueOr("secret", nil)) && !(has && issued == false) {
		return nil, &RelayRuntimeError{Msg: "connector returned an unexpected response (no secret)"}
	}
	return payload, nil
}

// Self provisions the relay at boot, true only when credentials were obtained, never failing for connector trouble
func SelfProvisionRelay(ctx context.Context) (bool, error) {
	outcome, err := relaySelfProvision(ctx)
	return outcome == relayProvisioned, err
}

// True when a relay is configured but this process holds no per gateway secret yet
func RelayProvisionPending(ctx context.Context) bool {
	if RelayURL(ctx) == "" {
		return false
	}
	existingID, existingSecret := RelayConnectionAuth(ctx)
	return !(existingID != "" && existingSecret != "")
}

// Self provision outcome of provisioned, skipped, refused or failed
func relaySelfProvision(ctx context.Context) (string, error) {
	dialURL, err := RelayURLChecked(ctx)
	if err != nil {
		return "", err
	}
	if dialURL == "" {
		return relaySkipped, nil
	}
	if existingID, existingSecret := RelayConnectionAuth(ctx); existingID != "" && existingSecret != "" {
		pkgLog().Info("relay self-provision skipped: GATEWAY_RELAY_SECRET already set")
		return relaySkipped, nil
	}
	accessToken, err := ResolveRelayIdentityToken(ctx)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("relay self-provision skipped: could not resolve identity token (%s)", relayExcStr(err)))
		return relayFailed, nil
	}
	identities := RelayPlatformIdentities()
	host, herr := os.Hostname()
	if herr != nil {
		host = ""
	}
	host = strings.TrimSpace(host)
	if host == "" {
		host = "icebath"
	}
	gatewayID := relaySetting(ctx, settings.GatewayRelayID)
	if gatewayID == "" {
		gatewayID = "gw-" + host
	}
	endpoint := RelayEndpoint(ctx)
	routeKeys := RelayRouteKeys(ctx)
	instanceID := RelayInstanceID(ctx)
	wakeURL := ""
	if u := RelayWakeURL(ctx); u != nil {
		wakeURL = *u
	}
	provisioned, refused, result, err := relayProvisionEachPlatform(ctx, identities, gatewayID, relayProvisionArgs{
		provisionURL: relayProvisionURL(dialURL), accessToken: accessToken, gatewayEndpoint: endpoint,
		routeKeys: routeKeys, instanceID: instanceID, wakeURL: wakeURL, displayName: RelayDisplayName(ctx),
	})
	if err != nil {
		return "", err
	}
	names := make([]string, len(identities))
	for i, id := range identities {
		names[i] = id[0]
	}
	if len(provisioned) == 0 {
		pkgLog().Warn(fmt.Sprintf("relay self-provision failed for ALL platforms (%s); gateway will boot without relay auth"+
			" and retry in the background", strings.Join(names, ",")))
		if refused == len(identities) {
			return relayRefused, nil
		}
		return relayFailed, nil
	}
	if relaySetting(ctx, settings.GatewayRelaySecret) == "" {
		pkgLog().Warn(fmt.Sprintf("relay self-provision withheld credentials for ALL platforms (%s): the connector "+
			"holds a binding for gateway_id=%s this caller could not prove ownership of; "+
			"gateway will boot without relay auth. Recover with POST /relay/rotate or pin "+
			"GATEWAY_RELAY_SECRET", strings.Join(provisioned, ","), gatewayID))
		return relayRefused, nil
	}
	loggedID := gatewayID
	if v := relaySetting(ctx, settings.GatewayRelayID); v != "" {
		loggedID = v
	}
	tenant := anyutil.String(anyutil.Or(result.ValueOr("tenant", nil), ""))
	if tenant == "" {
		tenant = "?"
	}
	inbound, instance, wake := "outbound-only", "unbound", "none"
	if endpoint != "" {
		inbound = "yes"
	}
	if instanceID != "" {
		instance = instanceID
	}
	if wakeURL != "" {
		wake = "yes"
	}
	pkgLog().Info(fmt.Sprintf("relay self-provisioned (gateway_id=%s tenant=%s platforms=%s routes=%d inbound=%s instance=%s wake=%s)",
		loggedID, tenant, strings.Join(provisioned, ","), len(routeKeys), inbound, instance, wake))
	return relayProvisioned, nil
}

// Provisions every fronted platform under one gateway id, saving the first credentials the connector hands out into the profile .env as icebath gateway enroll does, returning the provisioned names, refusal count and last result
func relayProvisionEachPlatform(ctx context.Context, identities [][2]string, gatewayID string, post relayProvisionArgs) ([]string, int, *jsonutil.Object, error) {
	provisioned := []string{}
	refused := 0
	result := jsonutil.NewObject()
	for _, id := range identities {
		args := post
		args.platform, args.botID, args.gatewayID = id[0], id[1], gatewayID
		payload, err := relayPostProvision(args)
		if err != nil {
			var refusal *RelayProvisionRefused
			var runtimeErr *RelayRuntimeError
			isRefusal := errors.As(err, &refusal)
			if !isRefusal && !errors.As(err, &runtimeErr) {
				return nil, 0, nil, err
			}
			if isRefusal {
				refused++
			}
			pkgLog().Warn(fmt.Sprintf("relay self-provision failed for platform=%s (%s); continuing with the rest", id[0], err.Error()))
			continue
		}
		result = payload
		provisioned = append(provisioned, id[0])
		if anyutil.IsTrue(result.ValueOr("secret", nil)) && relaySetting(ctx, settings.GatewayRelaySecret) == "" {
			for _, pair := range [][2]string{{"GATEWAY_RELAY_ID", anyutil.String(anyutil.Or(result.ValueOr("gatewayId", nil), gatewayID))},
				{"GATEWAY_RELAY_SECRET", anyutil.String(result.ValueOr("secret", nil))},
				{"GATEWAY_RELAY_DELIVERY_KEY", anyutil.String(anyutil.Or(result.ValueOr("deliveryKey", nil), ""))}} {
				if pair[1] == "" {
					continue
				}
				if err := config.SaveEnvValueContext(ctx, home.Dir(ctx), pair[0], pair[1]); err != nil {
					return nil, 0, nil, err
				}
			}
		}
	}
	return provisioned, refused, result, nil
}

// Retries provisioning until credentials exist, true only when this loop obtained them
func RetrySelfProvision(ctx context.Context, onProvisioned func()) (bool, error) {
	return RetrySelfProvisionWith(ctx, onProvisioned, sleepSeconds)
}

// Retry loop pacing each attempt through the given sleep
func RetrySelfProvisionWith(ctx context.Context, onProvisioned func(), sleep func(context.Context, float64) error) (bool, error) {
	attempt := 0
	delay := relayProvisionRetryDelays[0]
	for {
		if err := sleep(ctx, delay); err != nil {
			return false, err
		}
		if !RelayProvisionPending(ctx) {
			return false, nil
		}
		attempt++
		outcome, err := relayRunOffLoop(ctx, func() (string, error) { return relaySelfProvision(ctx) })
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("relay self-provision retry %d raised", attempt), "exc_info", err)
			outcome = relayFailed
		}
		if outcome == relayProvisioned {
			pkgLog().Info(fmt.Sprintf("relay credentials obtained on self-provision retry %d; reconnecting the relay", attempt))
			relayRunOffLoop(ctx, func() (string, error) {
				_, err := SendRelayPolicy(ctx)
				return "", err
			})
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if onProvisioned != nil {
				onProvisioned()
			}
			return true, nil
		}
		if outcome == relaySkipped {
			return false, nil
		}
		if outcome == relayRefused {
			delay = relayProvisionRefusalRetry
		} else {
			delay = relayProvisionRetryDelays[min(attempt, len(relayProvisionRetryDelays)-1)]
		}
		pkgLog().Info(fmt.Sprintf("relay self-provision retry %d %s; next attempt in %ds", attempt, outcome, int(delay)))
	}
}

// Runs blocking work on its own goroutine, a panic surfacing as an error and cancellation abandoning the wait
func relayRunOffLoop(ctx context.Context, fn func() (string, error)) (string, error) {
	type res struct {
		out string
		err error
	}
	done := make(chan res, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				if e, ok := rec.(error); ok {
					done <- res{err: e}
					return
				}
				done <- res{err: fmt.Errorf("%v", rec)}
			}
		}()
		out, err := fn()
		done <- res{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Python str of an exception value
func relayExcStr(err error) string {
	return err.Error()
}
