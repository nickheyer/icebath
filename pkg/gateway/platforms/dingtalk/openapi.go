package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Base URLs of the DingTalk OpenAPI and of the media upload API
var (
	openAPIBase = "https://api.dingtalk.com"
	oapiBase    = "https://oapi.dingtalk.com"
)

const (
	emotionID         = "2659900"
	emotionBackground = "im_bg_1"
	apiTimeout        = 30 * time.Second
	uploadTimeout     = 120 * time.Second
)

// Failure of a DingTalk API call with its HTTP status, service code and retry hint
type apiError struct {
	status     int
	code       string
	message    string
	retryAfter *float64
}

func (e *apiError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("DingTalk API error %s: %s", e.code, e.message)
	}
	return fmt.Sprintf("DingTalk API error HTTP %d: %s", e.status, e.message)
}

// Whether the API refused the call for flow control
func (e *apiError) rateLimited() bool {
	if e.status == http.StatusTooManyRequests {
		return true
	}
	switch e.code {
	case "130101", "410100", "90018":
		return true
	}
	blob := strings.ToLower(e.code + " " + e.message)
	for _, marker := range []string{"throttl", "qpslimit", "flowcontrol", "flow control", "too fast", "rate limit"} {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// Whether the token behind the call was refused
func (e *apiError) authRejected() bool {
	return e.status == http.StatusUnauthorized || strings.Contains(strings.ToLower(e.code), "invalidauthentication") ||
		e.code == "40014" || e.code == "42001"
}

// OpenAPI client of one app with its robot code and a cached access token
type openAPI struct {
	clientID     string
	clientSecret string
	robotCode    string
	http         *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Client for the app credentials, the robot code defaulting to the client id
func newOpenAPI(clientID, clientSecret, robotCode string, httpClient *http.Client) *openAPI {
	if robotCode == "" {
		robotCode = clientID
	}
	return &openAPI{clientID: clientID, clientSecret: clientSecret, robotCode: robotCode, http: httpClient}
}

// Access token of the app, cached until five minutes before it expires
func (c *openAPI) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	body := map[string]string{"appKey": c.clientID, "appSecret": c.clientSecret}
	if err := c.doJSON(ctx, http.MethodPost, openAPIBase+"/v1.0/oauth2/accessToken", "", body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("DingTalk returned no access token")
	}
	c.token = out.AccessToken
	c.expires = time.Now().Add(time.Duration(out.ExpireIn)*time.Second - 5*time.Minute)
	return c.token, nil
}

// Drops a token the API refused so the next call fetches a new one
func (c *openAPI) dropToken(token string) {
	c.mu.Lock()
	if c.token == token {
		c.token = ""
	}
	c.mu.Unlock()
}

// One JSON request decoding a success body into out and a failure body into an apiError
func (c *openAPI) doJSON(ctx context.Context, method, rawURL, token string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", provider.UserAgent("icebath"))
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("DingTalk API request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("DingTalk API response read failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		return parseAPIError(resp, raw)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("DingTalk API returned unreadable JSON: %w", err)
		}
	}
	return nil
}

// API error of a failed response, its code and message from the body and the Retry-After header
func parseAPIError(resp *http.Response, raw []byte) *apiError {
	e := &apiError{status: resp.StatusCode, message: clip(strings.TrimSpace(string(raw)), 300)}
	var body struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil {
		e.code = fmt.Sprint(body.Code)
		if body.Code == nil {
			e.code = ""
		}
		if body.Message != "" {
			e.message = body.Message
		}
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.ParseFloat(s, 64); err == nil && secs >= 0 {
			e.retryAfter = &secs
		}
	}
	if e.retryAfter == nil && e.rateLimited() {
		wait := 2.0
		e.retryAfter = &wait
	}
	return e
}

// Authenticated OpenAPI call, a refused token replaced once
func (c *openAPI) call(ctx context.Context, method, path string, body, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		err = c.doJSON(ctx, method, openAPIBase+path, token, body, out)
		var ae *apiError
		if attempt == 0 && errors.As(err, &ae) && ae.authRejected() {
			c.dropToken(token)
			continue
		}
		return err
	}
}

// Creates a streaming AI card instance of the template with empty content
func (c *openAPI) createCard(ctx context.Context, templateID, outTrackID string) error {
	body := map[string]any{"cardTemplateId": templateID, "outTrackId": outTrackID, "callbackType": "STREAM",
		"cardData":              map[string]any{"cardParamMap": map[string]string{"content": ""}},
		"imGroupOpenSpaceModel": map[string]any{"supportForward": true},
		"imRobotOpenSpaceModel": map[string]any{"supportForward": true}}
	return c.call(ctx, http.MethodPost, "/v1.0/card/instances", body, nil)
}

// Delivers a created card into a group conversation or a user's robot chat
func (c *openAPI) deliverCard(ctx context.Context, outTrackID string, t robotTarget) error {
	body := map[string]any{"outTrackId": outTrackID, "userIdType": 1}
	if t.conversationID != "" {
		body["openSpaceId"] = "dtv1.card//IM_GROUP." + t.conversationID
		body["imGroupOpenDeliverModel"] = map[string]any{"robotCode": c.robotCode}
	} else {
		body["openSpaceId"] = "dtv1.card//IM_ROBOT." + t.userID
		body["imRobotOpenDeliverModel"] = map[string]any{"spaceType": "IM_ROBOT"}
	}
	var out struct {
		Result []struct {
			SpaceID  string `json:"spaceId"`
			Success  *bool  `json:"success"`
			ErrorMsg string `json:"errorMsg"`
		} `json:"result"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1.0/card/instances/deliver", body, &out); err != nil {
		return err
	}
	for _, r := range out.Result {
		if r.Success != nil && !*r.Success {
			return fmt.Errorf("DingTalk card delivery to %s failed: %s", r.SpaceID, r.ErrorMsg)
		}
	}
	return nil
}

// Replaces the content of a streaming card, finalize closing its streaming state
func (c *openAPI) streamCard(ctx context.Context, outTrackID, content string, finalize bool) error {
	body := map[string]any{"outTrackId": outTrackID, "guid": uuid.NewString(), "key": "content", "content": content,
		"isFull": true, "isFinalize": finalize, "isError": false}
	return c.call(ctx, http.MethodPut, "/v1.0/card/streaming", body, nil)
}

// Adds or recalls a text emotion reaction on a message
func (c *openAPI) emotion(ctx context.Context, msgID, conversationID, name string, recall bool) error {
	path := "/v1.0/robot/emotion/reply"
	if recall {
		path = "/v1.0/robot/emotion/recall"
	}
	body := map[string]any{"robotCode": c.robotCode, "openMsgId": msgID, "openConversationId": conversationID,
		"emotionType": 2, "emotionName": name,
		"textEmotion": map[string]any{"emotionId": emotionID, "emotionName": name, "text": name, "backgroundId": emotionBackground}}
	return c.call(ctx, http.MethodPost, path, body, nil)
}

// Temporary download URL of an inbound file by its download code
func (c *openAPI) downloadURL(ctx context.Context, code, robotCode string) (string, error) {
	if robotCode == "" {
		robotCode = c.robotCode
	}
	var out struct {
		DownloadURL string `json:"downloadUrl"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1.0/robot/messageFiles/download",
		map[string]string{"downloadCode": code, "robotCode": robotCode}, &out); err != nil {
		return "", err
	}
	if out.DownloadURL == "" {
		return "", fmt.Errorf("DingTalk returned no download URL for code %s", clip(code, 24))
	}
	return out.DownloadURL, nil
}

// Recipient of an OpenAPI robot message, a group conversation or one user
type robotTarget struct {
	conversationID string
	userID         string
}

// Recipient read from a chat id, cid conversation ids as groups and anything else as a user id
func targetFromID(chatID string) robotTarget {
	if strings.HasPrefix(chatID, "cid") {
		return robotTarget{conversationID: chatID}
	}
	return robotTarget{userID: chatID}
}

// Sends one robot message template to the target, the process query key identifying it
func (c *openAPI) sendRobot(ctx context.Context, t robotTarget, msgKey string, param map[string]any) (string, error) {
	encoded, err := json.Marshal(param)
	if err != nil {
		return "", err
	}
	if t.conversationID != "" {
		var out struct {
			ProcessQueryKey string `json:"processQueryKey"`
		}
		body := map[string]any{"msgParam": string(encoded), "msgKey": msgKey, "openConversationId": t.conversationID, "robotCode": c.robotCode}
		if err := c.call(ctx, http.MethodPost, "/v1.0/robot/groupMessages/send", body, &out); err != nil {
			return "", err
		}
		return out.ProcessQueryKey, nil
	}
	if t.userID == "" {
		return "", errors.New("DingTalk robot message has no recipient")
	}
	var out struct {
		ProcessQueryKey    string   `json:"processQueryKey"`
		InvalidStaffIDs    []string `json:"invalidStaffIdList"`
		FlowControlledIDs  []string `json:"flowControlledStaffIdList"`
	}
	body := map[string]any{"robotCode": c.robotCode, "userIds": []string{t.userID}, "msgKey": msgKey, "msgParam": string(encoded)}
	if err := c.call(ctx, http.MethodPost, "/v1.0/robot/oToMessages/batchSend", body, &out); err != nil {
		return "", err
	}
	if slices.Contains(out.InvalidStaffIDs, t.userID) {
		return "", &apiError{status: http.StatusOK, code: "invalidStaffId", message: "DingTalk does not know user " + t.userID}
	}
	if slices.Contains(out.FlowControlledIDs, t.userID) {
		wait := 5.0
		return "", &apiError{status: http.StatusOK, code: "flowControlled", message: "DingTalk flow control held the message", retryAfter: &wait}
	}
	return out.ProcessQueryKey, nil
}

// Uploads a local file as image, voice, video or file media and returns its media id
func (c *openAPI) uploadMedia(ctx context.Context, mediaType, filePath, fileName string) (string, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("media", fileName)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	target := oapiBase + "/media/upload?access_token=" + url.QueryEscape(token) + "&type=" + url.QueryEscape(mediaType)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", provider.UserAgent("icebath"))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("DingTalk media upload failed: %w", redactToken(err, token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("DingTalk media upload response read failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", parseAPIError(resp, raw)
	}
	var out struct {
		ErrCode int64  `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("DingTalk media upload returned unreadable JSON: %w", err)
	}
	if out.ErrCode != 0 {
		if out.ErrCode == 40014 || out.ErrCode == 42001 {
			c.dropToken(token)
		}
		return "", &apiError{status: resp.StatusCode, code: strconv.FormatInt(out.ErrCode, 10), message: out.ErrMsg}
	}
	if out.MediaID == "" {
		return "", errors.New("DingTalk media upload returned no media id")
	}
	return out.MediaID, nil
}

// Error with the access token removed from any URL it quotes
func redactToken(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), url.QueryEscape(token), "***"))
}
