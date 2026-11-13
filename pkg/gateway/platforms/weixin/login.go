package weixin

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	qrBotType      = "3"
	qrLoginTimeout = 480 * time.Second
	qrMaxRefreshes = 3
)

// Bot credentials a confirmed QR login returns
type loginCredentials struct {
	AccountID string
	Token     string
	BaseURL   string
	UserID    string
}

// QR token and the liteapp URL WeChat scans
func fetchQR(ctx context.Context, client *http.Client) (string, string, error) {
	var resp struct {
		QRCode           flexString `json:"qrcode"`
		QRCodeImgContent flexString `json:"qrcode_img_content"`
	}
	err := apiGet(ctx, client, defaultBaseURL, epGetBotQR+"?bot_type="+url.QueryEscape(qrBotType), &resp, qrTimeout)
	return string(resp.QRCode), string(resp.QRCodeImgContent), err
}

// Prints the scan URL and a terminal QR code of it, naming a render failure when asked
func printQR(out io.Writer, value, scanURL string, reportRenderError bool) {
	if scanURL != "" {
		fmt.Fprintln(out, scanURL)
	}
	text := scanURL
	if text == "" {
		text = value
	}
	if _, err := qr.Encode(text, qr.M); err != nil {
		if reportRenderError {
			fmt.Fprintf(out, "（终端二维码渲染失败: %v，请直接打开上面的二维码链接）\n", err)
		}
		return
	}
	qrterminal.GenerateWithConfig(text, qrterminal.Config{Level: qrterminal.M, Writer: out, HalfBlocks: true,
		BlackChar: qrterminal.BLACK_BLACK, WhiteBlackChar: qrterminal.WHITE_BLACK, WhiteChar: qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE, QuietZone: 1})
}

// Context cancelled with ErrKeyboardInterrupt as its cause when Ctrl-C arrives
func interruptible(ctx context.Context) (context.Context, func()) {
	ictx, cancel := context.WithCancelCause(ctx)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		select {
		case <-sig:
			cancel(gateway.ErrKeyboardInterrupt)
		case <-ictx.Done():
		}
	}()
	return ictx, func() {
		signal.Stop(sig)
		cancel(nil)
	}
}

// Runs the iLink QR login on the setup console and saves the confirmed credentials, nil when it did not complete
func qrLogin(ctx context.Context, homeDir string) (*loginCredentials, error) {
	ctx, stop := interruptible(ctx)
	defer stop()
	out := provider.SetupOutput()
	client := newHTTPClient(ctx)
	defer client.CloseIdleConnections()
	value, scanURL, err := fetchQR(ctx, client)
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("weixin: failed to fetch QR code: %v", err))
		return nil, nil
	}
	if value == "" {
		slog.Error("weixin: QR response missing qrcode")
		return nil, nil
	}
	fmt.Fprintln(out, "\n请使用微信扫描以下二维码：")
	printQR(out, value, scanURL, true)
	deadline := time.Now().Add(qrLoginTimeout)
	baseURL, refreshes := defaultBaseURL, 0
	for time.Now().Before(deadline) {
		var status struct {
			Status       flexString `json:"status"`
			RedirectHost flexString `json:"redirect_host"`
			IlinkBotID   flexString `json:"ilink_bot_id"`
			BotToken     flexString `json:"bot_token"`
			BaseURL      flexString `json:"baseurl"`
			IlinkUserID  flexString `json:"ilink_user_id"`
		}
		err := apiGet(ctx, client, baseURL, epGetQRStatus+"?qrcode="+url.QueryEscape(value), &status, qrTimeout)
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if err != nil {
			if !isTimeout(err) {
				slog.Warn(fmt.Sprintf("weixin: QR poll error: %v", err))
			}
			if !sleepCtx(ctx, time.Second) {
				return nil, context.Cause(ctx)
			}
			continue
		}
		state := string(status.Status)
		if state == "" {
			state = "wait"
		}
		switch {
		case state == "wait":
			fmt.Fprint(out, ".")
		case state == "scaned":
			fmt.Fprintln(out, "\n已扫码，请在微信里确认...")
		case state == "scaned_but_redirect" && status.RedirectHost != "":
			baseURL = "https://" + string(status.RedirectHost)
		case state == "expired":
			refreshes++
			if refreshes > qrMaxRefreshes {
				fmt.Fprintln(out, "\n二维码多次过期，请重新执行登录。")
				return nil, nil
			}
			fmt.Fprintf(out, "\n二维码已过期，正在刷新... (%d/%d)\n", refreshes, qrMaxRefreshes)
			value, scanURL, err = fetchQR(ctx, client)
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			if err != nil {
				slog.Error(fmt.Sprintf("weixin: QR refresh failed: %v", err))
				return nil, nil
			}
			printQR(out, value, scanURL, false)
		case state == "confirmed":
			creds := &loginCredentials{AccountID: string(status.IlinkBotID), Token: string(status.BotToken),
				BaseURL: string(status.BaseURL), UserID: string(status.IlinkUserID)}
			if creds.BaseURL == "" {
				creds.BaseURL = defaultBaseURL
			}
			if creds.AccountID == "" || creds.Token == "" {
				slog.Error("weixin: QR confirmed but credential payload was incomplete")
				return nil, nil
			}
			if err := saveAccount(homeDir, creds.AccountID, creds.Token, creds.BaseURL, creds.UserID); err != nil {
				return nil, err
			}
			fmt.Fprintf(out, "\n微信连接成功，account_id=%s\n", creds.AccountID)
			return creds, nil
		}
		if !sleepCtx(ctx, time.Second) {
			return nil, context.Cause(ctx)
		}
	}
	fmt.Fprintln(out, "\n微信登录超时。")
	return nil, nil
}
