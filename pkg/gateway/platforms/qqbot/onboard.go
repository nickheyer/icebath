package qqbot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyqrcode"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
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

// Renders a QR code on the setup console, false when it cannot be rendered
func renderQR(url string) bool {
	code, err := pyqrcode.Make(url, pyqrcode.ErrorCorrectM, 2)
	if err != nil {
		return false
	}
	fmt.Fprint(provider.SetupOutput(), code.PrintASCII(true))
	return true
}

// Synchronous POST to the portal host, a RuntimeError for a non zero retcode
func portalPost(ctx context.Context, path string, payload any, timeout float64, failMsg string) (map[string]any, error) {
	body, err := jsonBody(payload)
	if err != nil {
		return nil, err
	}
	rawURL := "https://" + PortalHost + path
	client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), CheckRedirect: pyhttpx.RedirectPolicy(true)}
	defer client.CloseIdleConnections()
	resp, err := httpRequest(ctx, client, http.MethodPost, rawURL, GetAPIHeaders(), body, timeout)
	if err != nil {
		return nil, err
	}
	if err := raiseForStatus(resp, rawURL); err != nil {
		return nil, err
	}
	decoded, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	data, ok := asDict(decoded)
	if !ok {
		return nil, notDictErr(decoded)
	}
	if !equalsInt(data["retcode"], 0) {
		return nil, runtimeError(pyrepr.Str(getOr(data, "msg", failMsg)))
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
	inner, ok := asDict(orDefault(data["data"], map[string]any{}))
	if !ok {
		return "", "", notDictErr(data["data"])
	}
	taskID := inner["task_id"]
	if !pyrepr.Truthy(taskID) {
		return "", "", runtimeError("create_bind_task: missing task_id in response")
	}
	slog.Debug(fmt.Sprintf("create_bind_task ok: task_id=%s", pyrepr.Str(taskID)))
	return pyrepr.Str(taskID), key, nil
}

// Polls a bind task and returns its status, the bot app id, the encrypted bot secret and the scanner's openid
func pollBindResult(ctx context.Context, taskID string, timeout float64) (BindStatus, string, string, string, error) {
	data, err := portalPost(ctx, OnboardPollPath, obj("task_id", taskID), timeout, "poll_bind_result failed")
	if err != nil {
		return 0, "", "", "", err
	}
	d, ok := asDict(getOr(data, "data", map[string]any{}))
	if !ok {
		return 0, "", "", "", notDictErr(data["data"])
	}
	status, err := pyInt64(getOr(d, "status", int64(0)))
	if err != nil {
		return 0, "", "", "", err
	}
	if status < 0 || status > int64(BindStatusExpired) {
		return 0, "", "", "", &pyErr{"ValueError", fmt.Sprintf("%d is not a valid BindStatus", status)}
	}
	str := func(key string) string {
		if s, isStr := getOr(d, key, "").(string); isStr {
			return s
		}
		return pyrepr.Str(d[key])
	}
	return BindStatus(status), pyrepr.Str(getOr(d, "bot_appid", "")), str("bot_encrypt_secret"), str("user_openid"), nil
}

// QR code target URL of a bind task
func buildConnectURL(taskID string) string {
	return strings.ReplaceAll(QRURLTemplate, "{task_id}", pyurl.Quote(taskID, "/"))
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
			slog.Warn(fmt.Sprintf("[QQBot onboard] Failed to create bind task: %s", errText(err)))
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
			fmt.Fprintf(out, "  For a scannable QR code, run: %s\n", pm.InstallHint("messaging"))
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
					slog.Warn(fmt.Sprintf("[QQBot onboard] QR code expired %d times — giving up", maxOnboardRefreshes))
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
			slog.Warn(fmt.Sprintf("[QQBot onboard] Poll timed out after %ds", timeoutSeconds))
			return nil, nil
		}
	}
	return nil, nil
}
