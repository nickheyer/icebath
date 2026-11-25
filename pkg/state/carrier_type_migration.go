package state

import "context"

// Native assistant carrier type stored in reasoning details, and the spelling rows of older builds carry
const (
	nativeCarrierType        = "icebath.native_assistant"
	retiredNativeCarrierType = "hermes.native_assistant"
)

// Renames the retired carrier type inside one decoded reasoning value, reporting any change
func renameRetiredCarrierType(v any) bool {
	changed := false
	switch x := v.(type) {
	case orderedObject:
		for i := range x {
			if x[i].Key == "type" && x[i].Value == retiredNativeCarrierType {
				x[i].Value = nativeCarrierType
				changed = true
			} else if renameRetiredCarrierType(x[i].Value) {
				changed = true
			}
		}
	case []any:
		for _, item := range x {
			if renameRetiredCarrierType(item) {
				changed = true
			}
		}
	}
	return changed
}

// Rewrites stored reasoning details so native assistant carriers carry the current type
func renameStoredCarrierTypes(ctx context.Context, conn SQLiteConn) error {
	rows, err := queryObjects(ctx, conn, "SELECT id, reasoning_details FROM messages WHERE instr(reasoning_details, ?) > 0", retiredNativeCarrierType)
	if err != nil {
		return err
	}
	for _, row := range rows {
		id, _ := row.Get("id")
		raw, _ := row.Get("reasoning_details")
		var text string
		switch x := raw.(type) {
		case string:
			text = x
		case []byte:
			text = string(x)
		default:
			continue
		}
		v, err := loadsValue(text)
		if err != nil || !renameRetiredCarrierType(v) {
			continue
		}
		dumped, err := dumpsValue(v, jsonDefault)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE messages SET reasoning_details = ? WHERE id = ?", dumped, id); err != nil {
			return err
		}
	}
	return nil
}
