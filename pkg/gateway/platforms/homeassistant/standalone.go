package homeassistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// A notify service name, the form cron delivery targets take
var notifyServiceRE = func() interface{ MatchString(string) (bool, error) } {
	re, err := pyre.CompileFullmatch(`[a-z0-9_]+`)
	if err != nil {
		panic(err)
	}
	return re
}()

// Cron delivery through the target's notify service, grouped notify kept for an unknown service or a blank target
func standaloneSend(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	pconfig, _ := kwargs["pconfig"].(*gateway.PlatformConfig)
	chatID := kwargs["chat_id"]
	message := kwargs["message"]
	var extra map[string]any
	var token any
	if pconfig != nil {
		extra, token = pconfig.Extra, pconfig.Token
	}
	var rawURL any = extra["url"]
	if !pyrepr.Truthy(rawURL) {
		scoped, err := gateway.GetScopedSecret(ctx, "HASS_URL", "", false)
		if err != nil {
			return nil, err
		}
		rawURL = scoped
	}
	urlText, ok := rawURL.(string)
	if !ok {
		return nil, attrErr(rawURL, "the Home Assistant url", "text")
	}
	hassURL := strings.TrimRight(urlText, "/")
	if !pyrepr.Truthy(token) {
		scoped, err := gateway.GetScopedSecret(ctx, "HASS_TOKEN", "", false)
		if err != nil {
			return nil, err
		}
		token = scoped
	}
	tokenText, ok := token.(string)
	if !ok {
		return nil, attrErr(token, "the Home Assistant token", "text")
	}
	tokenText = strings.TrimSpace(tokenText)
	if hassURL == "" || tokenText == "" {
		return gateway.SendError("Home Assistant standalone send: HASS_URL and HASS_TOKEN must both be set"), nil
	}
	if problem := urlSchemeProblem(hassURL); problem != "" {
		return gateway.SendError("Home Assistant standalone send: " + problem), nil
	}
	type notifyCall struct {
		url     string
		payload *pyjson.Object
	}
	calls := []notifyCall{{hassURL + "/api/services/notify/notify", pyjson.ObjectOf("message", message, "target", chatID)}}
	var serviceRef any = chatID
	if !pyrepr.Truthy(serviceRef) {
		serviceRef = ""
	}
	serviceText, ok := serviceRef.(string)
	if !ok {
		return nil, attrErr(serviceRef, "the service", "text")
	}
	service := strings.TrimPrefix(strings.TrimSpace(serviceText), "notify.")
	if matched, err := notifyServiceRE.MatchString(service); err != nil {
		panic(err)
	} else if matched {
		calls = append([]notifyCall{{hassURL + "/api/services/notify/" + service, pyjson.ObjectOf("message", message)}}, calls...)
	}
	session := newSession(gateway.GatewayTrustEnv(ctx))
	defer session.CloseIdleConnections()
	for i := 0; ; i++ {
		call := calls[i]
		resp, err := postJSON(ctx, session, call.url, authHeaders(tokenText), call.payload, 30*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var timeout *timeoutError
			if errors.As(err, &timeout) {
				return gateway.SendError("Timeout sending notification to Home Assistant"), nil
			}
			return gateway.SendError("Home Assistant send failed: " + err.Error()), nil
		}
		if resp.status == 200 || resp.status == 201 {
			return map[string]any{"success": true, "platform": platformName, "chat_id": chatID}, nil
		}
		text, err := resp.text()
		if err != nil {
			return gateway.SendError("Home Assistant send failed: " + err.Error()), nil
		}
		errText := fmt.Sprintf("Home Assistant API error (%d) from %s: %s", resp.status, strings.TrimPrefix(call.url, hassURL), text)
		if resp.status != 400 || i == len(calls)-1 {
			return gateway.SendError(errText), nil
		}
	}
}
