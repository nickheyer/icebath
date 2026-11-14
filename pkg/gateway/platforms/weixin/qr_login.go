package weixin

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyqrcode"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Credentials a confirmed QR login returns
type qrCredentials struct {
	accountID, token, baseURL, userID string
}

// QR token and scannable liteapp URL of a fresh login code
func fetchQR(ctx context.Context, session *ilinkSession, botType string) (string, string, error) {
	resp, err := apiGet(ctx, session, ilinkBaseURL, epGetBotQR+"?bot_type="+botType, qrTimeoutMS)
	if err != nil {
		return "", "", err
	}
	return strOf(resp.Vals["qrcode"]), strOf(resp.Vals["qrcode_img_content"]), nil
}

// Prints the QR URL and its terminal rendering, WeChat scanning the liteapp URL and never the bare token
func printQR(qrcodeValue, qrcodeURL string, reportRenderError bool) {
	out := provider.SetupOutput()
	if qrcodeURL != "" {
		fmt.Fprintln(out, qrcodeURL)
	}
	data := qrcodeURL
	if data == "" {
		data = qrcodeValue
	}
	code, err := pyqrcode.Make(data, pyqrcode.ErrorCorrectM, 4)
	if err != nil {
		if reportRenderError {
			fmt.Fprintf(out, "（终端二维码渲染失败: %s，请直接打开上面的二维码链接）\n", errText(err))
		}
		return
	}
	fmt.Fprint(out, code.PrintASCII(true))
}

// Runs the iLink QR login, saving and returning the credentials once WeChat confirms, nil when it does not complete
func qrLogin(ctx context.Context, home, botType string, timeoutSeconds int) (*qrCredentials, error) {
	out := provider.SetupOutput()
	session := newSession(ctx)
	defer session.close()
	qrcodeValue, qrcodeURL, err := fetchQR(ctx, session, botType)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Error(fmt.Sprintf("weixin: failed to fetch QR code: %s", errText(err)))
		return nil, nil
	}
	if qrcodeValue == "" {
		slog.Error("weixin: QR response missing qrcode")
		return nil, nil
	}
	fmt.Fprintln(out, "\n请使用微信扫描以下二维码：")
	printQR(qrcodeValue, qrcodeURL, true)
	deadline := monotonic() + float64(timeoutSeconds)
	currentBaseURL, refreshCount := ilinkBaseURL, 0
	for monotonic() < deadline {
		statusResp, err := apiGet(ctx, session, currentBaseURL, epGetQRStatus+"?qrcode="+qrcodeValue, qrTimeoutMS)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !isTimeout(err) {
				slog.Warn(fmt.Sprintf("weixin: QR poll error: %s", errText(err)))
			}
			if err := sleep(ctx, qrPollGap); err != nil {
				return nil, err
			}
			continue
		}
		status := strOf(statusResp.Vals["status"])
		if status == "" {
			status = "wait"
		}
		switch {
		case status == "wait":
			fmt.Fprint(out, ".")
		case status == "scaned":
			fmt.Fprintln(out, "\n已扫码，请在微信里确认...")
		case status == "scaned_but_redirect" && pyrepr.Truthy(statusResp.Vals["redirect_host"]):
			currentBaseURL = "https://" + pyrepr.Str(statusResp.Vals["redirect_host"])
		case status == "expired":
			refreshCount++
			if refreshCount > 3 {
				fmt.Fprintln(out, "\n二维码多次过期，请重新执行登录。")
				return nil, nil
			}
			fmt.Fprintf(out, "\n二维码已过期，正在刷新... (%d/3)\n", refreshCount)
			if qrcodeValue, qrcodeURL, err = fetchQR(ctx, session, botType); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				slog.Error(fmt.Sprintf("weixin: QR refresh failed: %s", errText(err)))
				return nil, nil
			}
			printQR(qrcodeValue, qrcodeURL, false)
		case status == "confirmed":
			return confirmedCredentials(ctx, home, statusResp)
		}
		if err := sleep(ctx, qrPollGap); err != nil {
			return nil, err
		}
	}
	fmt.Fprintln(out, "\n微信登录超时。")
	return nil, nil
}

// Saves and returns the credentials of a confirmed login, nil when the payload lacks the account id or token
func confirmedCredentials(ctx context.Context, home string, statusResp *pyjson.Object) (*qrCredentials, error) {
	creds := &qrCredentials{accountID: strOf(statusResp.Vals["ilink_bot_id"]), token: strOf(statusResp.Vals["bot_token"]),
		baseURL: strOf(statusResp.Vals["baseurl"]), userID: strOf(statusResp.Vals["ilink_user_id"])}
	if creds.baseURL == "" {
		creds.baseURL = ilinkBaseURL
	}
	if creds.accountID == "" || creds.token == "" {
		slog.Error("weixin: QR confirmed but credential payload was incomplete")
		return nil, nil
	}
	if err := saveWeixinAccount(ctx, home, creds.accountID, creds.token, creds.baseURL, creds.userID); err != nil {
		return nil, err
	}
	fmt.Fprintf(provider.SetupOutput(), "\n微信连接成功，account_id=%s\n", creds.accountID)
	return creds, nil
}
