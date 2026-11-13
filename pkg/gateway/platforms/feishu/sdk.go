package feishu

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Open Platform calls the adapter makes, the lark SDK client in production
type larkAPI interface {
	CreateMessage(ctx context.Context, req *larkim.CreateMessageReq) (*larkim.CreateMessageResp, error)
	ReplyMessage(ctx context.Context, req *larkim.ReplyMessageReq) (*larkim.ReplyMessageResp, error)
	UpdateMessage(ctx context.Context, req *larkim.UpdateMessageReq) (*larkim.UpdateMessageResp, error)
	DeleteMessage(ctx context.Context, req *larkim.DeleteMessageReq) (*larkim.DeleteMessageResp, error)
	GetMessage(ctx context.Context, req *larkim.GetMessageReq) (*larkim.GetMessageResp, error)
	ListMessage(ctx context.Context, req *larkim.ListMessageReq) (*larkim.ListMessageResp, error)
	GetChat(ctx context.Context, req *larkim.GetChatReq) (*larkim.GetChatResp, error)
	CreateImage(ctx context.Context, req *larkim.CreateImageReq) (*larkim.CreateImageResp, error)
	CreateFile(ctx context.Context, req *larkim.CreateFileReq) (*larkim.CreateFileResp, error)
	GetMessageResource(ctx context.Context, req *larkim.GetMessageResourceReq) (*larkim.GetMessageResourceResp, error)
	CreateReaction(ctx context.Context, req *larkim.CreateMessageReactionReq) (*larkim.CreateMessageReactionResp, error)
	DeleteReaction(ctx context.Context, req *larkim.DeleteMessageReactionReq) (*larkim.DeleteMessageReactionResp, error)
	GetUser(ctx context.Context, req *larkcontact.GetUserReq) (*larkcontact.GetUserResp, error)
	GetApplication(ctx context.Context, req *larkapplication.GetApplicationReq) (*larkapplication.GetApplicationResp, error)
	tools.FeishuAPI
	Native() *lark.Client
}

type sdkClient struct{ c *lark.Client }

func (s sdkClient) CreateMessage(ctx context.Context, req *larkim.CreateMessageReq) (*larkim.CreateMessageResp, error) {
	return s.c.Im.V1.Message.Create(ctx, req)
}
func (s sdkClient) ReplyMessage(ctx context.Context, req *larkim.ReplyMessageReq) (*larkim.ReplyMessageResp, error) {
	return s.c.Im.V1.Message.Reply(ctx, req)
}
func (s sdkClient) UpdateMessage(ctx context.Context, req *larkim.UpdateMessageReq) (*larkim.UpdateMessageResp, error) {
	return s.c.Im.V1.Message.Update(ctx, req)
}
func (s sdkClient) DeleteMessage(ctx context.Context, req *larkim.DeleteMessageReq) (*larkim.DeleteMessageResp, error) {
	return s.c.Im.V1.Message.Delete(ctx, req)
}
func (s sdkClient) GetMessage(ctx context.Context, req *larkim.GetMessageReq) (*larkim.GetMessageResp, error) {
	return s.c.Im.V1.Message.Get(ctx, req)
}
func (s sdkClient) ListMessage(ctx context.Context, req *larkim.ListMessageReq) (*larkim.ListMessageResp, error) {
	return s.c.Im.V1.Message.List(ctx, req)
}
func (s sdkClient) GetChat(ctx context.Context, req *larkim.GetChatReq) (*larkim.GetChatResp, error) {
	return s.c.Im.V1.Chat.Get(ctx, req)
}
func (s sdkClient) CreateImage(ctx context.Context, req *larkim.CreateImageReq) (*larkim.CreateImageResp, error) {
	return s.c.Im.V1.Image.Create(ctx, req)
}
func (s sdkClient) CreateFile(ctx context.Context, req *larkim.CreateFileReq) (*larkim.CreateFileResp, error) {
	return s.c.Im.V1.File.Create(ctx, req)
}
func (s sdkClient) GetMessageResource(ctx context.Context, req *larkim.GetMessageResourceReq) (*larkim.GetMessageResourceResp, error) {
	return s.c.Im.V1.MessageResource.Get(ctx, req)
}
func (s sdkClient) CreateReaction(ctx context.Context, req *larkim.CreateMessageReactionReq) (*larkim.CreateMessageReactionResp, error) {
	return s.c.Im.V1.MessageReaction.Create(ctx, req)
}
func (s sdkClient) DeleteReaction(ctx context.Context, req *larkim.DeleteMessageReactionReq) (*larkim.DeleteMessageReactionResp, error) {
	return s.c.Im.V1.MessageReaction.Delete(ctx, req)
}
func (s sdkClient) GetUser(ctx context.Context, req *larkcontact.GetUserReq) (*larkcontact.GetUserResp, error) {
	return s.c.Contact.V3.User.Get(ctx, req)
}
func (s sdkClient) GetApplication(ctx context.Context, req *larkapplication.GetApplicationReq) (*larkapplication.GetApplicationResp, error) {
	return s.c.Application.V6.Application.Get(ctx, req)
}
func (s sdkClient) Do(ctx context.Context, req *larkcore.ApiReq) (*larkcore.ApiResp, error) {
	return s.c.Do(ctx, req)
}
func (s sdkClient) Native() *lark.Client { return s.c }

// Raw tenant token GET of a bot v3 endpoint, which has no typed SDK request
func tenantGetRequest(uri string, queries [][2]string) *larkcore.ApiReq {
	req := &larkcore.ApiReq{HttpMethod: http.MethodGet, ApiPath: uri, PathParams: larkcore.PathParams{}, QueryParams: larkcore.QueryParams{},
		SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant}}
	for _, q := range queries {
		req.QueryParams.Add(q[0], q[1])
	}
	return req
}

// Open Platform base URL of a domain name, Lark for lark and Feishu otherwise
func sdkDomain(domainName string) string {
	if domainName == "lark" {
		return lark.LarkBaseUrl
	}
	return lark.FeishuBaseUrl
}

// Lark client for the credentials and base URL, logging at warning level through slog
func buildLarkClient(appID, appSecret, domain string) larkAPI {
	return sdkClient{lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(domain), lark.WithLogLevel(larkcore.LogLevelWarn),
		lark.WithLogger(slogLarkLogger{}))}
}

// SDK logger writing through slog under the Lark prefix
type slogLarkLogger struct{}

func larkLine(args []any) string { return "[Lark] " + fmt.Sprint(args...) }

func (slogLarkLogger) Debug(ctx context.Context, args ...any) { slog.DebugContext(ctx, larkLine(args)) }
func (slogLarkLogger) Info(ctx context.Context, args ...any)  { slog.InfoContext(ctx, larkLine(args)) }
func (slogLarkLogger) Warn(ctx context.Context, args ...any)  { slog.WarnContext(ctx, larkLine(args)) }
func (slogLarkLogger) Error(ctx context.Context, args ...any) { slog.ErrorContext(ctx, larkLine(args)) }

// Button element with a plain text label, type and callback value
func cardButton(label, btnType string, value *pyjson.Object) *pyjson.Object {
	text := pyjson.NewObject()
	text.Set("tag", "plain_text")
	text.Set("content", label)
	out := pyjson.NewObject()
	out.Set("tag", "button")
	out.Set("text", text)
	out.Set("type", btnType)
	out.Set("value", value)
	return out
}

// Wide card with a colored header, one markdown block and an optional button row
func buildCard(title, template, markdown string, actions []any) *pyjson.Object {
	md := pyjson.NewObject()
	md.Set("tag", "markdown")
	md.Set("content", markdown)
	elements := []any{md}
	if actions != nil {
		row := pyjson.NewObject()
		row.Set("tag", "action")
		row.Set("actions", actions)
		elements = append(elements, row)
	}
	config := pyjson.NewObject()
	config.Set("wide_screen_mode", true)
	titleObj := pyjson.NewObject()
	titleObj.Set("content", title)
	titleObj.Set("tag", "plain_text")
	header := pyjson.NewObject()
	header.Set("title", titleObj)
	header.Set("template", template)
	out := pyjson.NewObject()
	out.Set("config", config)
	out.Set("header", header)
	out.Set("elements", elements)
	return out
}

// Response envelope of an SDK call, the zero value standing for a missing response
type apiOutcome struct {
	present bool
	code    int
	msg     string
	raw     *larkcore.ApiResp
}

// Code, message and raw HTTP response of any typed SDK response
func outcomeOf(resp any) apiOutcome {
	v := reflect.ValueOf(resp)
	if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return apiOutcome{}
	}
	e := reflect.Indirect(v)
	out := apiOutcome{present: true}
	if f := e.FieldByName("CodeError"); f.IsValid() {
		if ce, ok := f.Interface().(larkcore.CodeError); ok {
			out.code, out.msg = ce.Code, ce.Msg
		}
	}
	if f := e.FieldByName("ApiResp"); f.IsValid() {
		out.raw, _ = f.Interface().(*larkcore.ApiResp)
	}
	return out
}

func (o apiOutcome) success() bool { return o.present && o.code == 0 }

// Code of the response, unknown when there is none
func (o apiOutcome) codeOr() any {
	if !o.present {
		return "unknown"
	}
	return o.code
}

// Message of the response, def when there is none
func (o apiOutcome) msgOr(def string) string {
	if !o.present {
		return def
	}
	return o.msg
}

// First value of a response header, lowercased and cut at the first semicolon
func (o apiOutcome) header(name string) string {
	if o.raw == nil {
		return ""
	}
	return normalizeMediaType(o.raw.Header.Get(name), "")
}

// Body bytes of a binary download response
func readBinaryResponse(file io.Reader) []byte {
	if file == nil {
		return nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil
	}
	return data
}
