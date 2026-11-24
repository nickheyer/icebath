package wecom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	httputil "github.com/nickheyer/icebath/pkg/utils/http"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	qrutil "github.com/nickheyer/icebath/pkg/utils/qr"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Admin console endpoints of the QR bot creation, which back the console rather than the public API
const (
	qrGenerateURL  = "https://work.weixin.qq.com/ai/qc/generate"
	qrQueryURL     = "https://work.weixin.qq.com/ai/qc/query_result"
	qrCodePage     = "https://work.weixin.qq.com/ai/qc/gen?source=icebath&scode="
	qrPollInterval = 3.0
	qrPollTimeout  = 300
)

// Fetches and decodes a JSON document, a seam tests replace
var qrGetJSON = func(ctx context.Context, url string, timeout time.Duration) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "icebath-agent/1.0")
	resp, err := (&http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), Proxy: httputil.Proxy}}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return jsonutil.Decode(raw)
}

// Sleep between polls that a Ctrl-C cuts short, a seam tests replace
var qrSleep = func(w *gateway.InterruptWatch, seconds float64) error { return w.Sleep(seconds) }

// Monotonic seconds of the poll deadline
var qrClockBase = time.Now()
var qrMonotonic = func() float64 { return time.Since(qrClockBase).Seconds() }

// Value under key of a decoded JSON object, an error for anything that is not an object
func dictGet(v any, key string) (any, error) {
	o, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("expected a mapping, got %s", errchain.Kind(v))
	}
	return o.Value(key), nil
}

// The value, an empty object in place of an empty or missing one
func orDict(v any) any {
	if !anyutil.IsTrue(v) {
		return jsonutil.NewObject()
	}
	return v
}

// Trimmed text of a value, empty for an empty or missing one
func textOf(v any) string {
	if !anyutil.IsTrue(v) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(v))
}

// Fetches and renders a WeCom QR code then polls until scanned, the bot id and secret when it succeeds
func qrScanForBotInfo(ctx context.Context, timeoutSeconds int) (string, string, bool, error) {
	out := provider.SetupOutput()
	watch := gateway.WatchInterrupt()
	defer watch.Stop()
	fail := func(logMsg string, detail any, shown string) (string, string, bool, error) {
		pkgLog().Error(fmt.Sprintf(logMsg, anyutil.String(detail)))
		fmt.Fprintf(out, " failed: %s\n", shown)
		return "", "", false, nil
	}
	fmt.Fprint(out, "  Connecting to WeCom...")
	raw, err := qrGetJSON(ctx, qrGenerateURL+"?source=icebath", 15*time.Second)
	if err != nil {
		return fail("WeCom QR: failed to fetch QR code: %s", err.Error(), err.Error())
	}
	if err := watch.Check(); err != nil {
		return "", "", false, err
	}
	data, err := dictGet(raw, "data")
	if err != nil {
		return "", "", false, err
	}
	data = orDict(data)
	scodeRaw, err := dictGet(data, "scode")
	if err != nil {
		return "", "", false, err
	}
	authRaw, err := dictGet(data, "auth_url")
	if err != nil {
		return "", "", false, err
	}
	scode, authURL := textOf(scodeRaw), textOf(authRaw)
	if scode == "" || authURL == "" {
		return fail("WeCom QR: unexpected response format: %s", raw, "unexpected response format")
	}
	fmt.Fprint(out, " done.\n\n")
	pageURL := qrCodePage + urlutil.Escape(scode, "/")
	if code, err := qrutil.Encode(authURL, qrutil.Medium); err == nil {
		fmt.Fprint(out, code.HalfBlocks(4, true))
		fmt.Fprintf(out, "\n  Scan the QR code above, or open this URL directly:\n  %s\n", pageURL)
	} else {
		fmt.Fprintf(out, "  Open this URL in WeCom on your phone:\n\n  %s\n\n", pageURL)
	}
	fmt.Fprint(out, "\n  Fetching configuration results...")
	deadline := qrMonotonic() + float64(timeoutSeconds)
	queryURL := qrQueryURL + "?scode=" + urlutil.Escape(scode, "/")
	for qrMonotonic() < deadline {
		result, err := qrGetJSON(ctx, queryURL, 10*time.Second)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("WeCom QR poll error: %s", err.Error()))
			result = jsonutil.NewObject()
		} else {
			fmt.Fprint(out, ".")
		}
		if err := watch.Check(); err != nil {
			return "", "", false, err
		}
		resultData, err := dictGet(result, "data")
		if err != nil {
			return "", "", false, err
		}
		resultData = orDict(resultData)
		status, err := dictGet(resultData, "status")
		if err != nil {
			return "", "", false, err
		}
		if strings.ToLower(textOf(status)) != "success" {
			if err := qrSleep(watch, qrPollInterval); err != nil {
				return "", "", false, err
			}
			continue
		}
		botInfo, err := dictGet(resultData, "bot_info")
		if err != nil {
			return "", "", false, err
		}
		botInfo = orDict(botInfo)
		botid, _ := dictGet(botInfo, "botid")
		if !anyutil.IsTrue(botid) {
			botid, _ = dictGet(botInfo, "bot_id")
		}
		secretRaw, _ := dictGet(botInfo, "secret")
		botID, secret := textOf(botid), textOf(secretRaw)
		if botID != "" && secret != "" {
			fmt.Fprintln(out)
			return botID, secret, true, nil
		}
		pkgLog().Warn(fmt.Sprintf("WeCom QR: scan reported success but bot_info missing or incomplete: %s", anyutil.String(resultData)))
		fmt.Fprintln(out, "\n  QR scan reported success but no bot credentials were returned.\n"+
			"  This usually means the bot was not actually created on the WeCom side.\n  Falling back to manual credential entry.")
		return "", "", false, nil
	}
	fmt.Fprintf(out, "\n  QR scan timed out (%d minutes). Please try again.\n", timeoutSeconds/60)
	return "", "", false, nil
}

// QR bot creation of the wizard, a seam tests replace
var qrScanFn = qrScanForBotInfo

// Steps of finding an existing smart robot's credentials
var manualSetupSteps = []string{
	"1. Go to WeCom Application → Workspace → Smart Robot -> Create smart robots",
	"2. Select API Mode",
	"3. Copy the Bot ID and Secret from the bot's credentials info",
	"4. The bot connects via WebSocket — no public endpoint needed",
}

// Menu label, env saves and printed lines per unauthorized user choice, index 3 skipping
var accessChoices = []struct {
	label    string
	saves    [][2]string
	messages [][2]string
}{
	{"Enable open access (anyone can message the bot)", [][2]string{{"WECOM_DM_POLICY", "open"}, {"GATEWAY_ALLOW_ALL_USERS", "true"}},
		[][2]string{{"warning", "Open access enabled — anyone can use your bot!"}}},
	{"Use DM pairing (unknown users request access, you approve with 'icebath pairing approve')", [][2]string{{"WECOM_DM_POLICY", "pairing"}},
		[][2]string{{"success", "DM pairing mode — users will receive a code to request access."}, {"info", "Approve with: icebath pairing approve <platform> <code>"}}},
	{"Disable direct messages", [][2]string{{"WECOM_DM_POLICY", "disabled"}}, [][2]string{{"warning", "Direct messages disabled."}}},
	{"Skip for now (bot will deny all users until configured)", nil, [][2]string{{"info", "Skipped — configure later with 'icebath gateway setup'"}}},
}

// WeCom wizard of icebath gateway setup, the QR scan or manual Bot ID and Secret, access policy and home chat
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	cli.PrintHeader("WeCom (Enterprise WeChat)")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "WeCom", "Reconfigure WeCom?", "WECOM_BOT_ID")
	if err != nil || declined {
		return err
	}
	method := gateway.WizardSetup.PromptChoice("How would you like to set up WeCom?",
		[]string{"Scan QR code to obtain Bot ID and Secret automatically (recommended)", "Enter existing Bot ID and Secret manually"}, 0)
	botID, secret := "", ""
	if method == 0 {
		id, sec, ok, err := qrScanFn(ctx, qrPollTimeout)
		switch {
		case errors.Is(err, gateway.ErrKeyboardInterrupt):
			cli.PrintWarning("WeCom setup cancelled.")
			return nil
		case err != nil:
			cli.PrintWarning("QR scan failed: " + err.Error())
		case ok:
			botID, secret = id, sec
			cli.PrintSuccess("✔ QR scan successful! Bot ID and Secret obtained.")
		}
		if botID == "" || secret == "" {
			cli.PrintInfo("QR scan did not complete. Continuing with manual input.")
			botID, secret = "", ""
		}
	}
	if botID == "" || secret == "" {
		for _, line := range manualSetupSteps {
			cli.PrintInfo(line)
		}
		var creds []string
		for _, f := range []struct {
			label    string
			password bool
		}{{"Bot ID", false}, {"Secret", true}} {
			value := cli.Prompt(f.label, "", f.password)
			if value == "" {
				cli.PrintWarning("Skipped — WeCom won't work without a " + f.label + ".")
				return nil
			}
			creds = append(creds, value)
		}
		botID, secret = creds[0], creds[1]
	}
	if err := cfg.SaveEnvValue(ctx, "WECOM_BOT_ID", botID); err != nil {
		return err
	}
	if err := cfg.SaveEnvValue(ctx, "WECOM_SECRET", secret); err != nil {
		return err
	}
	cli.PrintInfo("The gateway DENIES all users by default for security.")
	cli.PrintInfo("Enter user IDs to create an allowlist, or leave empty.")
	if allowed := cli.Prompt("Allowed user IDs (comma-separated, or empty)", "", false); allowed != "" {
		if err := cfg.SaveEnvValue(ctx, "WECOM_ALLOWED_USERS", strings.ReplaceAll(allowed, " ", "")); err != nil {
			return err
		}
		cli.PrintSuccess("Saved — only these users can interact with the bot.")
	} else {
		labels := make([]string, len(accessChoices))
		for i, c := range accessChoices {
			labels[i] = c.label
		}
		idx := gateway.WizardSetup.PromptChoice("How should unauthorized users be handled?", labels, 1)
		if idx < 0 || idx > 2 {
			idx = 3
		}
		choice := accessChoices[idx]
		for _, kv := range choice.saves {
			if err := cfg.SaveEnvValue(ctx, kv[0], kv[1]); err != nil {
				return err
			}
		}
		for _, m := range choice.messages {
			map[string]func(string){"warning": cli.PrintWarning, "success": cli.PrintSuccess, "info": cli.PrintInfo}[m[0]](m[1])
		}
	}
	if home := strings.TrimSpace(cli.Prompt("Home chat ID (optional, for cron/notifications)", "", false)); home != "" {
		if err := cfg.SaveEnvValue(ctx, "WECOM_HOME_CHANNEL", home); err != nil {
			return err
		}
		cli.PrintSuccess("Home channel set to " + home)
	} else if removed, err := cfg.RemoveEnvValue(ctx, "WECOM_HOME_CHANNEL"); err != nil {
		return err
	} else if removed {
		cli.PrintInfo("Home channel cleared.")
	}
	cli.PrintSuccess("💬 WeCom configured!")
	return nil
}
