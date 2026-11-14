package dingtalk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// TeaException the OpenAPI gateway raises for a 4xx or 5xx answer, its str in the Tea format
type teaError struct {
	code    string
	message string
	data    *pyjson.Object
}

func (e *teaError) Error() string {
	return fmt.Sprintf("Error: %s %s Response: %s", e.code, e.message, pyrepr.Repr(e.data))
}

// First value that is set, the Tea default_any of a capitalized key and its lowercase twin
func defaultAny(obj *pyjson.Object, first, second string) any {
	if v, ok := obj.Get(first); ok && v != nil {
		return v
	}
	v, _ := obj.Get(second)
	return v
}

// DingTalk OpenAPI client for the card and robot calls, one HTTPS request per call with the app access token
type openAPIClient struct {
	http *http.Client
}

// Client over the central OpenAPI endpoint
func newOpenAPIClient() *openAPIClient {
	return &openAPIClient{http: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}}
}

// Sends one ROA request with a JSON body and returns the decoded JSON answer, a 4xx or 5xx raised as the Tea error
func (c *openAPIClient) call(ctx context.Context, method, pathname string, body *pyjson.Object, token any) (*pyjson.Object, error) {
	encoded, err := pyjson.Dumps(body, pyjson.EnsureASCII(false))
	if err != nil {
		return nil, err
	}
	target := "https://api.dingtalk.com" + pathname
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader([]byte(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("content-type", "application/json; charset=utf-8")
	req.Header.Set("x-acs-dingtalk-access-token", pyrepr.Str(token))
	resp, err := pyhttpx.DoTimeout(c.http, req, pyhttpx.Timeout{Connect: 5 * time.Second, Read: 10 * time.Second, Write: 10 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New(pyhttpx.ErrorText(err))
	}
	decode := func() (*pyjson.Object, error) {
		parsed, err := pyjson.LoadsBytes(resp.Content)
		if err != nil {
			return nil, err
		}
		obj, ok := parsed.(*pyjson.Object)
		if !ok {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: fmt.Sprintf("%s is not a dict", pyrepr.Repr(parsed))}
		}
		return obj, nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		failure, err := decode()
		if err != nil {
			return nil, err
		}
		failure.Set("statusCode", resp.StatusCode)
		return nil, &teaError{code: pyrepr.Str(defaultAny(failure, "Code", "code")),
			message: fmt.Sprintf("code: %d, %s request id: %s", resp.StatusCode, pyrepr.Str(defaultAny(failure, "Message", "message")),
				pyrepr.Str(defaultAny(failure, "RequestId", "requestid"))),
			data: failure}
	}
	if resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(resp.Content)) == 0 {
		return pyjson.NewObject(), nil
	}
	return decode()
}

// Creates an AI card instance from a template with empty content and stream callbacks
func (c *openAPIClient) createCard(ctx context.Context, token any, cardTemplateID any, outTrackID string) error {
	body := pyjson.ObjectOf("callbackType", "STREAM", "cardData", pyjson.ObjectOf("cardParamMap", pyjson.ObjectOf("content", "")),
		"cardTemplateId", cardTemplateID, "imGroupOpenSpaceModel", pyjson.ObjectOf("supportForward", true),
		"imRobotOpenSpaceModel", pyjson.ObjectOf("supportForward", true), "outTrackId", outTrackID)
	_, err := c.call(ctx, http.MethodPost, "/v1.0/card/instances", body, token)
	return err
}

// Delivers a card instance to a group or robot open space
func (c *openAPIClient) deliverCard(ctx context.Context, token any, outTrackID, openSpaceID, deliverKey string, deliverModel *pyjson.Object) error {
	body := pyjson.ObjectOf(deliverKey, deliverModel, "openSpaceId", openSpaceID, "outTrackId", outTrackID, "userIdType", 1)
	_, err := c.call(ctx, http.MethodPost, "/v1.0/card/instances/deliver", body, token)
	return err
}

// Streams the full content of a card, finalizing it when asked
func (c *openAPIClient) streamingUpdate(ctx context.Context, token any, outTrackID, guid, content string, finalize bool) error {
	body := pyjson.ObjectOf("content", content, "guid", guid, "isError", false, "isFinalize", finalize, "isFull", true, "key", "content",
		"outTrackId", outTrackID)
	_, err := c.call(ctx, http.MethodPut, "/v1.0/card/streaming", body, token)
	return err
}

// Adds or recalls a text emotion reaction of the robot on a message
func (c *openAPIClient) robotEmotion(ctx context.Context, token any, recall bool, robotCode any, openMsgID, openConversationID, emojiName string) error {
	textEmotion := pyjson.ObjectOf("backgroundId", emotionBG, "emotionId", emotionID, "emotionName", emojiName, "text", emojiName)
	body := pyjson.ObjectOf("emotionName", emojiName, "emotionType", 2, "openConversationId", openConversationID, "openMsgId", openMsgID,
		"robotCode", robotCode, "textEmotion", textEmotion)
	pathname := "/v1.0/robot/emotion/reply"
	if recall {
		pathname = "/v1.0/robot/emotion/recall"
	}
	_, err := c.call(ctx, http.MethodPost, pathname, body, token)
	return err
}

// Download URL of a robot message file code, the answer body nil when the gateway returned none
func (c *openAPIClient) robotMessageFileDownload(ctx context.Context, token any, downloadCode, robotCode any) (*pyjson.Object, error) {
	body := pyjson.ObjectOf("downloadCode", downloadCode, "robotCode", robotCode)
	return c.call(ctx, http.MethodPost, "/v1.0/robot/messageFiles/download", body, token)
}
