package qqbot

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	qrutil "github.com/nickheyer/icebath/pkg/utils/qr"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Status of a bind task as the portal reports it
type BindStatus int

const (
	BindStatusNone BindStatus = iota
	BindStatusPending
	BindStatusCompleted
	BindStatusExpired
)

const maxOnboardRefreshes = 3

// Credentials a completed QR registration yields
type onboardCredentials struct {
	appID, clientSecret, userOpenID string
}

// Renders a QR code with the four module quiet zone the QR standard asks for on the setup console, false when it cannot be rendered
func renderQR(target string) bool {
	code, err := qrutil.Encode(target, qrutil.Medium)
	if err != nil {
		return false
	}
	fmt.Fprint(provider.SetupOutput(), code.HalfBlocks(4, true))
	return true
}

// Synchronous POST to the portal host, a RuntimeError for a non zero retcode
func portalPost(ctx context.Context, path string, payload any, timeout float64, failMsg string) (map[string]any, error) {
	body, err := jsonBody(payload)
	if err != nil {
		return nil, err
	}
	rawURL := "https://" + portalHost(ctx) + path
	client := &http.Client{Transport: netpref.Follow(http.DefaultTransport.(*http.Transport).Clone()), CheckRedirect: httputil.RedirectPolicy(true)}
	defer client.CloseIdleConnections()
	resp, err := httpRequest(ctx, client, http.MethodPost, rawURL, GetAPIHeaders(), body, timeout)
	if err != nil {
		return nil, err
	}
	if err := resp.CheckStatus(); err != nil {
		return nil, err
	}
	decoded, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	data, ok := jsonutil.AsMap(decoded)
	if !ok {
		return nil, notDictErr(decoded)
	}
	if !equalsInt(data["retcode"], 0) {
		return nil, runtimeError(anyutil.String(getOr(data, "msg", failMsg)))
	}
	return data, nil
}

// Creates a bind task and returns its id with the base64 AES key that decrypts the bot secret
func createBindTask(ctx context.Context, timeout float64) (string, string, error) {
	key, err := GenerateBindKey()
	if err != nil {
		return "", "", err
	}
	data, err := portalPost(ctx, OnboardCreatePath, obj("key", key), timeout, "create_bind_task failed")
	if err != nil {
		return "", "", err
	}
	inner, ok := jsonutil.AsMap(orDefault(data["data"], map[string]any{}))
	if !ok {
		return "", "", notDictErr(data["data"])
	}
	taskID := inner["task_id"]
	if !anyutil.IsTrue(taskID) {
		return "", "", runtimeError("create_bind_task: missing task_id in response")
	}
	pkgLog().Debug(fmt.Sprintf("create_bind_task ok: task_id=%s", anyutil.String(taskID)))
	return anyutil.String(taskID), key, nil
}

// Polls a bind task and returns its status, the bot app id, the encrypted bot secret and the scanner's openid
func pollBindResult(ctx context.Context, taskID string, timeout float64) (BindStatus, string, string, string, error) {
	data, err := portalPost(ctx, OnboardPollPath, obj("task_id", taskID), timeout, "poll_bind_result failed")
	if err != nil {
		return 0, "", "", "", err
	}
	d, ok := jsonutil.AsMap(getOr(data, "data", map[string]any{}))
	if !ok {
		return 0, "", "", "", notDictErr(data["data"])
	}
	status, err := anyutil.ToInt(getOr(d, "status", int64(0)))
	if err != nil {
		return 0, "", "", "", err
	}
	if status < 0 || status > int64(BindStatusExpired) {
		return 0, "", "", "", fmt.Errorf("bind status %d is out of range", status)
	}
	str := func(key string) string {
		if s, isStr := getOr(d, key, "").(string); isStr {
			return s
		}
		return anyutil.String(d[key])
	}
	return BindStatus(status), anyutil.String(getOr(d, "bot_appid", "")), str("bot_encrypt_secret"), str("user_openid"), nil
}

// QR code target URL of a bind task
func buildConnectURL(taskID string) string {
	return strings.ReplaceAll(QRURLTemplate, "{task_id}", urlutil.Escape(taskID, "/"))
}

// Runs the QR registration flow, nil when it fails, expires past the refresh cap or times out
func qrRegister(ctx context.Context, timeoutSeconds int) (*onboardCredentials, error) {
	out := provider.SetupOutput()
	watch := gateway.WatchInterrupt()
	defer watch.Stop()
	deadline := monotonic() + float64(timeoutSeconds)
	for refreshCount := 0; refreshCount <= maxOnboardRefreshes; refreshCount++ {
		taskID, aesKey, err := createBindTask(ctx, OnboardAPITimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			pkgLog().Warn(fmt.Sprintf("[QQBot onboard] Failed to create bind task: %v", err))
			return nil, nil
		}
		if err := watch.Check(); err != nil {
			return nil, err
		}
		url := buildConnectURL(taskID)
		fmt.Fprintln(out)
		if renderQR(url) {
			fmt.Fprintf(out, "  Scan the QR code above, or open this URL directly:\n  %s\n", url)
		} else {
			fmt.Fprintf(out, "  Open this URL in QQ on your phone:\n  %s\n", url)
			fmt.Fprintln(out, "  This link is too long to show as a QR code.")
		}
		fmt.Fprintln(out)
		expired := false
		for !expired && monotonic() < deadline {
			status, appID, encryptedSecret, userOpenID, err := pollBindResult(ctx, taskID, OnboardAPITimeout)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if err := watch.Sleep(OnboardPollInterval); err != nil {
					return nil, err
				}
				continue
			}
			switch status {
			case BindStatusCompleted:
				clientSecret, err := DecryptSecret(encryptedSecret, aesKey)
				if err != nil {
					return nil, err
				}
				fmt.Fprintln(out)
				fmt.Fprintf(out, "  QR scan complete! (App ID: %s)\n", appID)
				if userOpenID != "" {
					fmt.Fprintf(out, "  Scanner's OpenID: %s\n", userOpenID)
				}
				return &onboardCredentials{appID: appID, clientSecret: clientSecret, userOpenID: userOpenID}, nil
			case BindStatusExpired:
				if refreshCount >= maxOnboardRefreshes {
					pkgLog().Warn(fmt.Sprintf("[QQBot onboard] QR code expired %d times — giving up", maxOnboardRefreshes))
					return nil, nil
				}
				fmt.Fprintf(out, "\n  QR code expired, refreshing... (%d/%d)\n", refreshCount+1, maxOnboardRefreshes)
				expired = true
			default:
				if err := watch.Sleep(OnboardPollInterval); err != nil {
					return nil, err
				}
			}
		}
		if !expired {
			pkgLog().Warn(fmt.Sprintf("[QQBot onboard] Poll timed out after %ds", timeoutSeconds))
			return nil, nil
		}
	}
	return nil, nil
}
