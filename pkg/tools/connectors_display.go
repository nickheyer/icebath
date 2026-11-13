package tools

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// OffDesktopNote tells the model the connector card exists only in the desktop app
const OffDesktopNote = "The connector card shows only in the icebath desktop app. Tell the user to connect apps from the " +
	"desktop app or the portal."

// DisplayState marks a display card operation with its suggested slugs and the words the user typed
type DisplayState struct {
	Suggested []string
	said      string
}

// ConnectorLister reads the gateway's connector list, the one call the display card makes
type ConnectorLister interface {
	ListConnectors(ctx context.Context, timeout time.Duration) ([]map[string]any, error)
}

// Applies the card's answer, settling on Continue or on words typed in the composer
func DisplayApplyAnswer(op *ConnectionOperation, raw string) error {
	parsed, err := pyjson.Loads(raw)
	if err != nil {
		return err
	}
	answer, ok := parsed.(*pyjson.Object)
	if !ok {
		return fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(parsed))
	}
	said := ""
	if v, _ := answer.Get("said"); connTruthy(v) {
		said = pystr.Strip(pyrepr.Str(v))
	}
	if said != "" {
		op.mu.Lock()
		op.Display.said = said
		op.mu.Unlock()
	}
	if by, _ := answer.Get("settled_by"); said != "" || by == string(SettleContinue) {
		op.Settle(SettleContinue)
	}
	return nil
}

// Lowercased slugs of every connector the list reports connected
func ConnectedSlugs(ctx context.Context, client ConnectorLister) (map[string]bool, error) {
	items, err := client.ListConnectors(ctx, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, item := range items {
		if item != nil && truthy(item["connected"]) {
			out[connectorSlug(item)] = true
		}
	}
	return out, nil
}

// Lowercased str of an item's connector field, empty when the key is absent
func connectorSlug(item map[string]any) string {
	v, ok := item["connector"]
	if !ok {
		return ""
	}
	if v == nil {
		return "none"
	}
	return pystr.Lower(pyrepr.Str(v))
}

func displayResult(ctx context.Context, client ConnectorLister, before map[string]bool, op *ConnectionOperation) *pyjson.Object {
	after, err := ConnectedSlugs(ctx, client)
	if err != nil {
		connRaise(err)
	}
	var connected, fresh []string
	for slug := range after {
		connected = append(connected, slug)
		if !before[slug] {
			fresh = append(fresh, slug)
		}
	}
	sort.Strings(connected)
	sort.Strings(fresh)
	result := pyjson.ObjectOf("connected", connAnyList(connected), "newly_connected", connAnyList(fresh), "suggested", connAnyList(op.Display.Suggested))
	op.mu.Lock()
	said := op.Display.said
	op.mu.Unlock()
	if said != "" {
		result.Set("said", said)
	}
	if op.SettledBy() == SettleDeadline {
		result.Set("timed_out", true)
	}
	return result
}

// Draws the browsable connector card and holds the tool call until it settles
func RunDisplay(ctx context.Context, env *Env, client ConnectorLister, suggested []string, sessionKey, toolCallID string, cb ConnCallback, call any) string {
	before, err := ConnectedSlugs(ctx, client)
	if err != nil {
		connRaise(err)
	}
	op := NewConnectionOperation(nil, sessionKey, toolCallID, call)
	op.Display = &DisplayState{Suggested: suggested}
	unchanged := func(*ConnectionOperation) {}
	kind := ConnKind{Prepare: unchanged, Observe: unchanged, Result: func(*ConnectionOperation) *pyjson.Object {
		return displayResult(ctx, client, before, op)
	}}
	return OpenAndDrive(ctx, env, op, kind, cb, nil, false)
}

// Python list of strings, an empty list rather than null
func connAnyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}
