package wecom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Admin console endpoints of QR bot creation, which back the console rather than the public API
const (
	qrGenerateURL  = "https://work.weixin.qq.com/ai/qc/generate?source=icebath"
	qrQueryURL     = "https://work.weixin.qq.com/ai/qc/query_result?scode="
	qrCodePage     = "https://work.weixin.qq.com/ai/qc/gen?source=icebath&scode="
	qrPollInterval = 3.0
	qrPollTimeout  = 5 * time.Minute
	qrBorder       = 4
)

// Decoded JSON object of a console endpoint fetched through the environment proxy
func qrGetJSON(ctx context.Context, rawURL string, timeout time.Duration) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "icebath-agent/1.0")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	client := &http.Client{Timeout: timeout, Transport: transport}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

// QR code of the text in half block characters for a dark terminal, with a four module quiet zone
func renderQR(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	n := code.Size
	module := func(row, col int) int {
		if row >= n+qrBorder || col >= n+qrBorder {
			return 1
		}
		if code.Black(col, row) {
			return 1
		}
		return 0
	}
	glyphs := []string{"█", "▄", "▀", " "}
	var b strings.Builder
	for row := -qrBorder; row < n+qrBorder; row += 2 {
		for col := -qrBorder; col < n+qrBorder; col++ {
			b.WriteString(glyphs[module(row, col)+module(row+1, col)<<1])
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// Fetches and shows a WeCom QR code then polls until scanned, the bot id and secret when the scan creates a bot
func qrScanForBotInfo(ctx context.Context, timeout time.Duration) (string, string, error) {
	out := provider.SetupOutput()
	watch := gateway.WatchInterrupt()
	defer watch.Stop()
	fmt.Fprint(out, "  Connecting to WeCom...")
	raw, err := qrGetJSON(ctx, qrGenerateURL, 15*time.Second)
	if err != nil {
		slog.Error(fmt.Sprintf("WeCom QR: failed to fetch QR code: %v", err))
		fmt.Fprintf(out, " failed: %v\n", err)
		return "", "", nil
	}
	if err := watch.Check(); err != nil {
		return "", "", err
	}
	data := objAt(raw, "data")
	scode, authURL := strAt(data, "scode"), strAt(data, "auth_url")
	if scode == "" || authURL == "" {
		slog.Error(fmt.Sprintf("WeCom QR: unexpected response format: %v", raw))
		fmt.Fprintln(out, " failed: unexpected response format")
		return "", "", nil
	}
	fmt.Fprint(out, " done.\n\n")
	pageURL := qrCodePage + url.QueryEscape(scode)
	if code, err := renderQR(authURL); err == nil {
		fmt.Fprint(out, code)
		fmt.Fprintf(out, "\n  Scan the QR code above, or open this URL directly:\n  %s\n", pageURL)
	} else {
		fmt.Fprintf(out, "  Open this URL in WeCom on your phone:\n\n  %s\n\n", pageURL)
	}
	fmt.Fprint(out, "\n  Fetching configuration results...")
	deadline := time.Now().Add(timeout)
	queryURL := qrQueryURL + url.QueryEscape(scode)
	for time.Now().Before(deadline) {
		result, err := qrGetJSON(ctx, queryURL, 10*time.Second)
		if err != nil {
			slog.Debug(fmt.Sprintf("WeCom QR poll error: %v", err))
			result = map[string]any{}
		} else {
			fmt.Fprint(out, ".")
		}
		if err := watch.Check(); err != nil {
			return "", "", err
		}
		resultData := objAt(result, "data")
		if strings.ToLower(strAt(resultData, "status")) != "success" {
			if err := watch.Sleep(qrPollInterval); err != nil {
				return "", "", err
			}
			continue
		}
		botInfo := objAt(resultData, "bot_info")
		botID := strAt(botInfo, "botid")
		if botID == "" {
			botID = strAt(botInfo, "bot_id")
		}
		if secret := strAt(botInfo, "secret"); botID != "" && secret != "" {
			fmt.Fprintln(out)
			return botID, secret, nil
		}
		slog.Warn(fmt.Sprintf("WeCom QR: scan reported success but bot_info missing or incomplete: %v", resultData))
		fmt.Fprintln(out, "\n  QR scan reported success but no bot credentials were returned.\n"+
			"  This usually means the bot was not actually created on the WeCom side.\n  Falling back to manual credential entry.")
		return "", "", nil
	}
	fmt.Fprintf(out, "\n  QR scan timed out (%d minutes). Please try again.\n", int(timeout.Minutes()))
	return "", "", nil
}

// Steps of finding an existing smart robot's credentials
var manualSetupSteps = []string{
	"1. Go to WeCom Application → Workspace → Smart Robot -> Create smart robots",
	"2. Select API Mode",
	"3. Copy the Bot ID and Secret from the bot's credentials info",
	"4. The bot connects via WebSocket — no public endpoint needed",
}

// Menu label, env saves and printed lines per unauthorized user choice, the last one skipping
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
		id, sec, err := qrScanForBotInfo(ctx, qrPollTimeout)
		switch {
		case errors.Is(err, gateway.ErrKeyboardInterrupt):
			cli.PrintWarning("WeCom setup cancelled.")
			return nil
		case err != nil:
			cli.PrintWarning("QR scan failed: " + err.Error())
		case id != "" && sec != "":
			botID, secret = id, sec
			cli.PrintSuccess("✔ QR scan successful! Bot ID and Secret obtained.")
		}
		if botID == "" || secret == "" {
			cli.PrintInfo("QR scan did not complete. Continuing with manual input.")
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
		if idx < 0 || idx >= len(accessChoices) {
			idx = len(accessChoices) - 1
		}
		choice := accessChoices[idx]
		for _, kv := range choice.saves {
			if err := cfg.SaveEnvValue(ctx, kv[0], kv[1]); err != nil {
				return err
			}
		}
		show := map[string]func(string){"warning": cli.PrintWarning, "success": cli.PrintSuccess, "info": cli.PrintInfo}
		for _, m := range choice.messages {
			show[m[0]](m[1])
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
