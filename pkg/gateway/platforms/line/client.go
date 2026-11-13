package line

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Messaging API and content API hosts the client talks to
var (
	lineAPIEndpoint  = "https://api.line.me"
	lineDataEndpoint = "https://api-data.line.me"
)

const (
	lineReplyPath   = "/v2/bot/message/reply"
	linePushPath    = "/v2/bot/message/push"
	lineDefaultWait = 15 * time.Second
)

// Messaging API client over the LINE SDK posting the reply and push bodies hermes sends
type lineClient struct {
	token   string
	timeout time.Duration
}

// Client for a channel access token with the 15 second request timeout
func newLineClient(token string) *lineClient {
	return &lineClient{token: token, timeout: lineDefaultWait}
}

// SDK messaging client bound to the call context with its own total timeout
func (c *lineClient) api(ctx context.Context, timeout time.Duration) (*messaging_api.MessagingApiAPI, error) {
	api, err := messaging_api.NewMessagingApiAPI(c.token,
		messaging_api.WithHTTPClient(gateway.TrustEnvHTTPClient(ctx, timeout)), messaging_api.WithEndpoint(lineAPIEndpoint))
	if err != nil {
		return nil, err
	}
	return api.WithContext(ctx), nil
}

// Posts a messages payload, an error naming the call, status and first 200 body chars past 400
func (c *lineClient) postMessages(ctx context.Context, path, label string, payload map[string]any) error {
	api, err := c.api(ctx, c.timeout)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, api.Url(path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := api.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		text, _ := io.ReadAll(res.Body)
		return fmt.Errorf("LINE %s %d: %s", label, res.StatusCode, pystr.Slice(pystr.DecodeUTF8Replace(text), 0, 200))
	}
	return nil
}

// Answers an inbound event with its single use reply token
func (c *lineClient) reply(ctx context.Context, replyToken string, messages []messaging_api.MessageInterface) error {
	return c.postMessages(ctx, lineReplyPath, "reply", map[string]any{"replyToken": replyToken, "messages": messages})
}

// Sends metered push messages to a chat
func (c *lineClient) push(ctx context.Context, chatID string, messages []messaging_api.MessageInterface) error {
	return c.postMessages(ctx, linePushPath, "push", map[string]any{"to": chatID, "messages": messages})
}

// Starts the DM loading animation in 5 second steps up to 60, never raising
func (c *lineClient) loading(ctx context.Context, chatID string, seconds int) {
	if chatID == "" || !strings.HasPrefix(chatID, "U") {
		return
	}
	stepped := (seconds / 5) * 5
	if seconds < 0 && seconds%5 != 0 {
		stepped -= 5
	}
	if stepped == 0 {
		stepped = 5
	}
	clamped := max(5, min(60, stepped))
	api, err := c.api(ctx, 5*time.Second)
	if err == nil {
		var res *http.Response
		res, _, err = api.ShowLoadingAnimationWithHttpInfo(&messaging_api.ShowLoadingAnimationRequest{ChatId: chatID, LoadingSeconds: int32(clamped)})
		if res != nil {
			err = nil
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("LINE loading indicator failed: %v", err))
	}
}

// Downloads the binary content of an inbound message
func (c *lineClient) fetchContent(ctx context.Context, messageID string) ([]byte, error) {
	blob, err := messaging_api.NewMessagingApiBlobAPI(c.token,
		messaging_api.WithBlobHTTPClient(gateway.TrustEnvHTTPClient(ctx, 30*time.Second)), messaging_api.WithBlobEndpoint(lineDataEndpoint))
	if err != nil {
		return nil, err
	}
	res, content, err := blob.WithContext(ctx).GetMessageContentWithHttpInfo(messageID)
	if res != nil && res.StatusCode >= 400 {
		res.Body.Close()
		return nil, fmt.Errorf("LINE content %d", res.StatusCode)
	}
	if err != nil {
		return nil, err
	}
	defer content.Body.Close()
	return io.ReadAll(content.Body)
}

// This channel's own userId for self echo filtering, empty on any failure
func (c *lineClient) getBotUserID(ctx context.Context) string {
	api, err := c.api(ctx, 10*time.Second)
	if err != nil {
		return ""
	}
	res, info, err := api.GetBotInfoWithHttpInfo()
	if err != nil || res == nil || res.StatusCode >= 400 || info == nil {
		return ""
	}
	return info.UserId
}
