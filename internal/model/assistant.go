package model

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

func (x *exchange) assemble(ctx context.Context, r *wire.Response) {
	m := &r.Message
	if m.Role == "" {
		m.Role = wire.RoleAssistant
	}
	reasoning := extractReasoning(m)
	if reasoning == "" {
		reasoning = plainThinkBlocks(m.FlatText(""))
	}
	m.Reasoning = reasoning
	if m.ReasoningContent == nil {
		switch {
		case len(m.ToolCalls) > 0 && x.needsReasoningPad(ctx):
			pad := reasoning
			if pad == "" {
				pad = " "
			}
			m.ReasoningContent = &pad
		case reasoning != "":
			promoted := reasoning
			m.ReasoningContent = &promoted
		}
	}
	if record := wire.CarrierRecord(m); record != nil && !hasCarrierDetail(m) {
		m.ReasoningDetails = append(m.ReasoningDetails, record)
	}
	for i := range m.ToolCalls {
		assignCallIDs(&m.ToolCalls[i], i)
	}
	m.FinishReason = r.FinishReason
}

func (x *exchange) needsReasoningPad(ctx context.Context) bool {
	rt := x.rt
	return x.s.r.ReasoningReplayRoute(ctx, rt.Mode, rt.Provider, rt.Model, rt.BaseURL, x.s.ReasoningEcho(), x.s.RejectedCarriers(ctx)).Pad
}

func hasCarrierDetail(m *wire.Message) bool {
	for _, d := range m.ReasoningDetails {
		if wire.DetailType(d) == wire.CarrierType {
			return true
		}
	}
	return false
}

func splitResponsesToolID(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	if call, item, found := strings.Cut(value, "|"); found {
		return strings.TrimSpace(call), strings.TrimSpace(item)
	}
	if value == "" {
		return "", ""
	}
	if strings.HasPrefix(value, "fc_") {
		return "", value
	}
	return value, ""
}

func assignCallIDs(c *wire.ToolCall, index int) {
	raw := c.ID
	callID := c.CallID
	if strings.TrimSpace(callID) == "" {
		callID, _ = splitResponsesToolID(raw)
	}
	if strings.TrimSpace(callID) == "" {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			callID = trimmed
		} else {
			callID = wire.DeterministicCallID(c.Name, c.Arguments, index)
		}
	}
	callID = strings.TrimSpace(callID)
	itemID := c.ResponseItemID
	if strings.TrimSpace(itemID) == "" {
		_, itemID = splitResponsesToolID(raw)
	}
	c.ID, c.CallID, c.ResponseItemID = callID, callID, deriveFunctionCallID(callID, itemID)
}

func deriveFunctionCallID(callID, itemID string) string {
	if trimmed := strings.TrimSpace(itemID); strings.HasPrefix(trimmed, "fc_") {
		return trimmed
	}
	source := strings.TrimSpace(callID)
	var b strings.Builder
	for _, r := range source {
		if r == '_' || r == '-' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	sanitized := b.String()
	for _, cand := range []string{source, sanitized} {
		if strings.HasPrefix(cand, "fc_") {
			return cand
		}
		if strings.HasPrefix(cand, "call_") && len(cand) > len("call_") {
			return "fc_" + cand[len("call_"):]
		}
	}
	if sanitized != "" {
		return "fc_" + sanitized[:min(48, len(sanitized))]
	}
	seed := source
	if seed == "" {
		seed = itemID
	}
	if seed == "" {
		var buf [16]byte
		_, _ = rand.Read(buf[:])
		seed = hex.EncodeToString(buf[:])
	}
	sum := sha1.Sum([]byte(seed))
	return "fc_" + hex.EncodeToString(sum[:])[:24]
}
