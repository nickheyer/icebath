package yuanbao

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ForwardedRecordsParseMiddleware renders forwarded chat records into the raw text and adds their media to the refs
type ForwardedRecordsParseMiddleware struct{ middlewareBase }

func newForwardedRecordsParseMiddleware() InboundMiddleware {
	return &ForwardedRecordsParseMiddleware{middlewareBase{"ForwardedRecordsParseMiddleware", "forwarded-records-parse"}}
}

// Cap on the rendered text of one forwarded record, the record count staying uncapped
const forwardMsgTextMaxChars = 1000

// Rewrites the raw text from the forwarded records, leaving it untouched when the parse fails
func (m *ForwardedRecordsParseMiddleware) Handle(ctx context.Context, ic *InboundContext, next func() error) error {
	if ic.ForwardedRecords != nil && ic.ForwardedRecords.Len() > 0 {
		sendLoadingHeartbeat(ctx, ic)
		text, err := buildForwardText(ic.ForwardedRecords, ic, true)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			slog.Warn(fmt.Sprintf("[%s] forwarded-records deep parse failed: %s", ic.Adapter.Name(), errText(err)))
		default:
			ic.RawText = text
		}
	}
	return next()
}

// Sends one RUNNING heartbeat so the user sees a loading bubble
func sendLoadingHeartbeat(ctx context.Context, ic *InboundContext) {
	ic.Adapter.outbound.heartbeat.sendHeartbeatOnce(ctx, ic.ChatID, WSHeartbeatRunning)
}

var forwardMediaKinds = map[string]string{"image": "image", "file": "file", "document": "file", "code": "file", "video": "video"}

// Marker text of one multimedia entry, with its media ref when a resource id and URL make it resolvable
func mediaMarker(media map[string]any, plainText any) (string, *InboundMediaRef, error) {
	typeText, err := strMethod(pyrepr.Or(dictGetOr(media, "type", ""), dictGetOr(media, "doc_type", "")), "strip")
	if err != nil {
		return "", nil, err
	}
	mediaType := pystr.Lower(pystr.Strip(typeText))
	url := stripOrEmpty(media["url"])
	name := stripOrEmpty(media["file_name"])
	rid := stripOrEmpty(media["media_id"])
	if rid == "" {
		rid = parseResourceID(url)
	}
	kind := forwardMediaKinds[mediaType]
	if kind != "" && url != "" && rid != "" {
		refName := ""
		if kind == "file" {
			refName = name
		}
		ref := mediaRef(kind, url, refName)
		return pystr.Rstrip("[" + kind + "|ybres:" + rid + "] " + name), &ref, nil
	}
	switch {
	case kind == "image":
		return pystr.Rstrip("[image] " + pyrepr.Str(pyrepr.Or(name, plainText))), nil, nil
	case kind == "file":
		return pystr.Rstrip("[file] " + name), nil, nil
	case kind == "video":
		return pystr.Rstrip("[video] " + pyrepr.Str(pyrepr.Or(name, url))), nil, nil
	case mediaType == "url":
		return pystr.Rstrip("[link] " + pyrepr.Str(pyrepr.Or(name, strOrEmpty(media["title"]))) + " " + url), nil, nil
	}
	return pystr.Rstrip("[" + pyrepr.Str(pyrepr.Or(mediaType, "media")) + "] " + pyrepr.Str(pyrepr.Or(url, name))), nil, nil
}

// Visits the sender, capped body and media refs of each forwarded record in order
func walkForwardMsgs(forwardData *pyjson.Object, visit func(sender any, body string, refs []InboundMediaRef)) error {
	var msgs any
	if forwardData != nil {
		msgs = forwardData.GetOr("msg", nil)
	}
	if !pyrepr.Truthy(msgs) {
		return nil
	}
	records, err := iterOf(msgs)
	if err != nil {
		return err
	}
	for _, record := range records {
		msg, isDict := asDict(record)
		if !isDict {
			continue
		}
		plainText := dictGetOr(msg, "plainText", "")
		refs := []InboundMediaRef{}
		parts := []any{}
		var contents []any
		if raw := dictGetOr(msg, "msgContent", []any{}); pyrepr.Truthy(raw) {
			if contents, err = iterOf(raw); err != nil {
				return err
			}
		}
		for _, item := range contents {
			mc, isDict := asDict(item)
			if !isDict {
				continue
			}
			mcType := dictGetOr(mc, "type", int64(0))
			switch {
			case equalsInt(mcType, 1):
				parts = append(parts, dictGetOr(mc, "text", ""))
			case equalsInt(mcType, 2):
				var medias []any
				if raw := dictGetOr(mc, "multimedia", []any{}); pyrepr.Truthy(raw) {
					if medias, err = iterOf(raw); err != nil {
						return err
					}
				}
				for _, entry := range medias {
					media, isDict := asDict(entry)
					if !isDict {
						continue
					}
					marker, ref, err := mediaMarker(media, plainText)
					if err != nil {
						return err
					}
					parts = append(parts, marker)
					if ref != nil {
						refs = append(refs, *ref)
					}
				}
			case equalsInt(mcType, 3):
				parts = append(parts, "[嵌套聊天记录]")
			case pyrepr.Truthy(plainText):
				parts = append(parts, plainText)
			}
		}
		truthy := make([]any, 0, len(parts))
		for _, p := range parts {
			if pyrepr.Truthy(p) {
				truthy = append(truthy, p)
			}
		}
		joined, err := joinParts("  ", truthy)
		if err != nil {
			return err
		}
		var rendered any = joined
		if joined == "" {
			rendered = plainText
		}
		body, isStr := rendered.(string)
		if !isStr {
			return &pyErr{"TypeError", fmt.Sprintf("object of type '%s' has no len()", pyrepr.TypeName(rendered))}
		}
		if pystr.Len(body) > forwardMsgTextMaxChars {
			body = pystr.Slice(body, 0, forwardMsgTextMaxChars) + "…(已截断)"
		}
		visit(dictGetOr(msg, "sender", ""), body, refs)
	}
	return nil
}

// Forwarded records as sender and body lines, a dispatch also adding the media refs and the user's own text as a footer
func buildForwardText(forwardData *pyjson.Object, ic *InboundContext, isDispatch bool) (string, error) {
	lines := []string{"当前用户的昵称为" + pyrepr.Str(pyrepr.Or(ic.SenderNickname, "用户")), "以下为用户的聊天记录"}
	err := walkForwardMsgs(forwardData, func(sender any, body string, refs []InboundMediaRef) {
		lines = append(lines, pyrepr.Str(sender)+"："+body)
		if isDispatch {
			ic.MediaRefs = append(ic.MediaRefs, refs...)
		}
	})
	if err != nil {
		return "", err
	}
	text := strings.Join(lines, "\n")
	if note := pystr.Strip(ic.RawText); isDispatch && note != "" {
		text += "\n\n用户附言：" + note
	}
	return text, nil
}
