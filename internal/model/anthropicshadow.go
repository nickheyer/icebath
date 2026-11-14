package model

import (
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/bedrock"
)

const applicationProfileMarker = ":application-inference-profile/"

var anthropicShadowKeys = []string{"content", "tool_calls", "reasoning_content", "cache_control"}

func findMember(members []wire.Member, key string) (wire.Member, bool) {
	for _, m := range members {
		if m.Key == key {
			return m, true
		}
	}
	return wire.Member{}, false
}

func (e *Estimator) anthropicProjection(msgs []wire.Message) ([][]wire.Member, []string, map[string]string) {
	lists := make([][]wire.Member, 0, len(msgs))
	var thinking []string
	carriers := map[string]string{}
	for i := range msgs {
		members := msgs[i].Members()
		for k, v := range msgs[i].Carriers {
			carriers[k] = v
		}
		if msgs[i].Role != wire.RoleAssistant {
			lists = append(lists, members)
			continue
		}
		shadow := []wire.Member{{Key: "role", Value: wire.Quote("assistant")}}
		for _, key := range anthropicShadowKeys {
			if m, ok := findMember(members, key); ok {
				shadow = append(shadow, m)
			}
		}
		if sidecar, ok := findMember(members, "api_content"); ok {
			if s, isString := memberValue(members, "api_content").Str(); isString && s != "" {
				replaced := false
				for j := range shadow {
					if shadow[j].Key == "content" {
						shadow[j].Value, replaced = sidecar.Value, true
					}
				}
				if !replaced {
					shadow = append(shadow, wire.Member{Key: "content", Value: sidecar.Value})
				}
			}
		}
		carrier, texts := e.replayCarrier(members)
		if carrier {
			shadow = withoutKeys(shadow, []string{"reasoning_content"})
		}
		thinking = append(thinking, texts...)
		lists = append(lists, shadow)
	}
	return lists, thinking, carriers
}

func blankTextBlock(b wire.Value) bool {
	text, ok := b.Get("text").Str()
	return !ok || strings.TrimSpace(text) == ""
}

func (e *Estimator) replayCarrier(members []wire.Member) (bool, []string) {
	ordered := memberValue(members, "anthropic_content_blocks")
	if ordered.IsArray() && len(ordered.Elems()) > 0 {
		var texts []string
		survivors, cacheable, droppedText, relocated := 0, false, false, false
		for _, b := range ordered.Elems() {
			kind, _ := b.Get("type").Str()
			survived := false
			if b.IsObject() {
				switch kind {
				case "text":
					survived = !blankTextBlock(b)
				case "thinking":
					survived = true
					if s, ok := b.Get("thinking").Str(); ok && s != "" {
						texts = append(texts, s)
					}
				case "redacted_thinking":
					survived = b.Get("data").Truthy()
				case "tool_use":
					survived = true
				case "image":
					survived = b.Get("source").IsObject()
				}
			}
			if !survived {
				droppedText = droppedText || (b.IsObject() && kind == "text")
				relocated = relocated || (b.IsObject() && b.Get("cache_control").IsObject())
				continue
			}
			survivors++
			cacheable = cacheable || kind == "text" || kind == "tool_use"
		}
		if survivors > 0 || (!cacheable && (droppedText || relocated)) {
			return true, texts
		}
	}
	details := memberValue(members, "reasoning_details")
	if !details.IsArray() || e.mintedOutsideClaude(details) {
		return false, nil
	}
	found := false
	var texts []string
	for _, d := range details.Elems() {
		if !d.IsObject() {
			continue
		}
		raw, _ := d.Get("type").Str()
		kind := strings.ToLower(strings.TrimSpace(raw))
		if kind != "thinking" && kind != "redacted_thinking" {
			continue
		}
		found = true
		if raw != "thinking" {
			continue
		}
		if s, ok := d.Get("thinking").Str(); ok && s != "" {
			texts = append(texts, s)
		}
	}
	return found, texts
}

func (e *Estimator) mintedOutsideClaude(details wire.Value) bool {
	issuer, found := "", false
	for _, d := range details.Elems() {
		if d.IsObject() && d.Get("type").Text() == bedrock.IssuerType && d.Get("model").IsString() {
			issuer, found = d.Get("model").Text(), true
			break
		}
	}
	if !found {
		return false
	}
	if strings.Contains(issuer, applicationProfileMarker) {
		resolved := e.resolve(issuer)
		if resolved == issuer {
			return false
		}
		issuer = resolved
	}
	return !strings.Contains(strings.ToLower(issuer), "claude")
}
