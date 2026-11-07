package codeexec

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	noTargetMsg        = "No active window — call capture() first."
	btfUnsupportedMsg  = "The connected cua-driver does not advertise the standalone bring_to_front tool."
	foregroundUnsupMsg = "The connected cua-driver action schema does not accept delivery_mode, so foreground " +
		"delivery is unavailable. Use another verified rung without assuming the reported " +
		"package version describes the live schema."
)

// pointerVariant is one addressing form, extra nil when the caller did not supply it, lazy when computing it probes
type pointerVariant struct {
	what  string
	extra *pyjson.Object
	lazy  func() *pyjson.Object
}

// Refused action result
func refuseAction(action, message string) *ActionResult {
	return &ActionResult{OK: false, Action: action, Message: message, Meta: pyjson.NewObject()}
}

// Refused action result carrying a code and an optional delivery mode
func refuseCoded(action, message, code, delivery string) *ActionResult {
	r := refuseAction(action, message)
	r.Code = &code
	if delivery != "" {
		r.DeliveryMode = &delivery
	}
	return r
}

// Base args of an input action against the sticky target, or the refusal when there is none
func (b *CuaDriverBackend) targetArgs(action string, needWindow bool) (*ActionResult, *pyjson.Object) {
	if b.activePID == nil || (needWindow && b.activeWindowID == nil) {
		return refuseAction(action, noTargetMsg), pyjson.NewObject()
	}
	args := pyjson.NewObject()
	args.Set("pid", *b.activePID)
	if needWindow {
		args.Set("window_id", *b.activeWindowID)
	}
	return nil, args
}

// Fills args from the first supplied addressing variant plus window_id, refusing without one when missing is set
func (b *CuaDriverBackend) pointerArgs(tool string, args *pyjson.Object, variants []pointerVariant, missing string) *ActionResult {
	for _, v := range variants {
		if v.extra == nil && v.lazy == nil {
			continue
		}
		if b.activeWindowID == nil {
			return refuseAction(tool, "No active window_id for "+v.what+".")
		}
		extra := v.extra
		if v.lazy != nil {
			extra = v.lazy()
		}
		for _, k := range extra.Keys {
			args.Set(k, extra.Vals[k])
		}
		args.Set("window_id", *b.activeWindowID)
		return nil
	}
	if missing != "" {
		return refuseAction(tool, missing)
	}
	return nil
}

// Attaches foreground delivery when the live schema accepts it, refusing unknown or unsupported modes
func (b *CuaDriverBackend) applyDelivery(action string, args *pyjson.Object, mode *string) *ActionResult {
	if mode == nil || *mode == "" || *mode == "background" {
		return nil
	}
	if *mode != "foreground" {
		return refuseCoded(action, fmt.Sprintf("unknown delivery_mode %s — use background|foreground.", pyrepr.Repr(*mode)), "bad_delivery_mode", "")
	}
	if !b.session.supportsInputProperty(action, "delivery_mode") {
		return refuseCoded(action, foregroundUnsupMsg, "foreground_unsupported", "foreground")
	}
	args.Set("delivery_mode", "foreground")
	return nil
}

// Applies one delivery rung, running the separately approved bring_to_front first when requested
func (b *CuaDriverBackend) runInputAction(action string, args *pyjson.Object, mode *string, bringToFront bool) *ActionResult {
	if refusal := b.applyDelivery(action, args, mode); refusal != nil {
		return refusal
	}
	if bringToFront {
		if mode == nil || *mode != "foreground" {
			return refuseCoded(action, "bring_to_front requires delivery_mode='foreground'.", "bring_to_front_requires_foreground", "")
		}
		if !b.session.hasTool("bring_to_front") {
			return refuseCoded(action, btfUnsupportedMsg, "bring_to_front_unsupported", "foreground")
		}
		if b.activePID == nil || b.activeWindowID == nil {
			return refuseCoded(action, "Capture an exact target before requesting persistent foreground focus.",
				"bring_to_front_target_required", "foreground")
		}
		if focused := b.BringToFront(*b.activePID, b.activeWindowID); !focused.OK {
			return focused
		}
	}
	result := b.action(action, args, true)
	if bringToFront {
		focus := pyjson.NewObject()
		focus.Set("invoked", true)
		focus.Set("tool", "bring_to_front")
		result.Meta.Set("foreground_focus", focus)
	}
	return result
}

// Single or double click by element index or coordinates with the button through click's enum
func (b *CuaDriverBackend) Click(a ClickArgs) (*ActionResult, error) {
	refusal, args := b.targetArgs("click", false)
	if refusal != nil {
		return refusal, nil
	}
	button := a.Button
	if button == "" {
		button = "left"
	}
	norm := pystr.Lower(button)
	if norm != "left" && norm != "right" && norm != "middle" {
		return refuseAction("click", fmt.Sprintf("unknown button %s — expected left, right, middle.", pyrepr.Repr(a.Button))), nil
	}
	tool := "click"
	if a.ClickCount == 2 {
		tool = "double_click"
	}
	args.Set("button", norm)
	var element, coords *pyjson.Object
	if a.Element != nil {
		element = pyjson.NewObject()
		element.Set("element_index", *a.Element)
	}
	if a.X != nil && a.Y != nil {
		coords = pyjson.NewObject()
		coords.Set("x", *a.X)
		coords.Set("y", *a.Y)
	}
	refusal = b.pointerArgs(tool, args, []pointerVariant{{what: "element_index click", extra: element},
		{what: "coordinate click", extra: coords}}, "click requires element= or x/y.")
	if len(a.Modifiers) > 0 {
		args.Set("modifier", stringsToAny(a.Modifiers))
	}
	if refusal != nil {
		return refusal, nil
	}
	return b.runInputAction(tool, args, a.DeliveryMode, a.BringToFront), nil
}

// Drag between two elements or two points
func (b *CuaDriverBackend) Drag(a DragArgs) (*ActionResult, error) {
	refusal, args := b.targetArgs("drag", false)
	if refusal == nil {
		var elems, coords *pyjson.Object
		if a.FromElement != nil && a.ToElement != nil {
			elems = pyjson.NewObject()
			elems.Set("from_element", *a.FromElement)
			elems.Set("to_element", *a.ToElement)
		}
		if a.FromXY != nil && a.ToXY != nil {
			coords = pyjson.NewObject()
			coords.Set("from_x", a.FromXY[0])
			coords.Set("from_y", a.FromXY[1])
			coords.Set("to_x", a.ToXY[0])
			coords.Set("to_y", a.ToXY[1])
		}
		refusal = b.pointerArgs("drag", args, []pointerVariant{{what: "element-based drag", extra: elems},
			{what: "coordinate drag", extra: coords}}, "drag requires from_element/to_element or from_coordinate/to_coordinate.")
	}
	if refusal != nil {
		return refusal, nil
	}
	return b.runInputAction("drag", args, a.DeliveryMode, a.BringToFront), nil
}

// Scroll by wheel ticks clamped to 1 through 50, coordinates only when the driver advertises them
func (b *CuaDriverBackend) Scroll(a ScrollArgs) (*ActionResult, error) {
	refusal, args := b.targetArgs("scroll", false)
	if refusal != nil {
		return refusal, nil
	}
	args.Set("direction", a.Direction)
	args.Set("amount", max(1, min(50, a.Amount)))
	var element *pyjson.Object
	if a.Element != nil && b.activeWindowID != nil {
		element = pyjson.NewObject()
		element.Set("element_index", *a.Element)
	}
	var lazy func() *pyjson.Object
	if a.X != nil && a.Y != nil {
		x, y := *a.X, *a.Y
		lazy = func() *pyjson.Object {
			out := pyjson.NewObject()
			if b.session.supportsCapability("input.scroll.coordinates", "scroll") {
				out.Set("x", x)
				out.Set("y", y)
			}
			return out
		}
	}
	refusal = b.pointerArgs("scroll", args, []pointerVariant{{what: "element scroll", extra: element},
		{what: "coordinate scroll", lazy: lazy}}, "")
	if refusal != nil {
		return refusal, nil
	}
	return b.runInputAction("scroll", args, a.DeliveryMode, a.BringToFront), nil
}

// Types text into the sticky target window
func (b *CuaDriverBackend) TypeText(text string, d DeliveryArgs) (*ActionResult, error) {
	refusal, args := b.targetArgs("type_text", true)
	if refusal != nil {
		return refusal, nil
	}
	args.Set("text", text)
	return b.runInputAction("type_text", args, d.DeliveryMode, d.BringToFront), nil
}

// Presses a key or a modifier combination through hotkey
func (b *CuaDriverBackend) Key(keys string, d DeliveryArgs) (*ActionResult, error) {
	refusal, args := b.targetArgs("key", true)
	if refusal != nil {
		return refusal, nil
	}
	keyName, modifiers := parseKeyCombo(keys)
	if keyName == nil || *keyName == "" {
		return refuseAction("key", fmt.Sprintf("Could not parse key from '%s'.", keys)), nil
	}
	if len(modifiers) > 0 {
		args.Set("keys", stringsToAny(append(append([]string(nil), modifiers...), *keyName)))
		return b.runInputAction("hotkey", args, d.DeliveryMode, d.BringToFront), nil
	}
	args.Set("key", *keyName)
	return b.runInputAction("press_key", args, d.DeliveryMode, d.BringToFront), nil
}

// Sets a value on an element, popup selects included
func (b *CuaDriverBackend) SetValue(value string, element *int) (*ActionResult, error) {
	refusal, args := b.targetArgs("set_value", true)
	if refusal != nil {
		return refusal, nil
	}
	if element == nil {
		return refuseAction("set_value", "set_value requires element= (element index)."), nil
	}
	args.Set("element_index", *element)
	args.Set("value", value)
	return b.action("set_value", args, true), nil
}

// Strings as a JSON list value
func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
