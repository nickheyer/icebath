package lsp

import "github.com/nickheyer/icebath/pkg/internal/pystr"

// LineShift maps a pre edit line to its post edit line, false when the line was deleted
type LineShift func(line int64) (int64, bool)

// BuildLineShift maps pre edit lines to post edit lines from the difflib opcodes of the two texts
func BuildLineShift(rt *Runtime, preText, postText string) (LineShift, error) {
	var pre, post []string
	if preText != "" {
		pre = pystr.Splitlines(preText, false)
	}
	if postText != "" {
		post = pystr.Splitlines(postText, false)
	}
	if equalLines(pre, post) {
		return func(line int64) (int64, bool) { return line, true }, nil
	}
	if rt == nil || rt.Opcodes == nil {
		return nil, missingHook("Opcodes")
	}
	opcodes, err := guard(func() ([]Opcode, error) { return rt.Opcodes(pre, post), nil })
	if err != nil {
		return nil, err
	}
	postLen := int64(len(post))
	return func(line int64) (int64, bool) {
		for _, op := range opcodes {
			i1, i2, j1 := int64(op.I1), int64(op.I2), int64(op.J1)
			if i1 <= line && line < i2 {
				if op.Tag == "equal" {
					return line - i1 + j1, true
				}
				return 0, false
			}
			if line < i1 {
				break
			}
		}
		if postLen > 0 {
			return max(0, postLen-1), true
		}
		return 0, false
	}, nil
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ShiftDiagnosticRange copies a diagnostic with its line range remapped, false when its start line was deleted
func ShiftDiagnosticRange(diag map[string]any, shift LineShift) (map[string]any, bool, error) {
	rng, err := dictOr(diag["range"])
	if err != nil {
		return nil, false, err
	}
	start, err := dictOr(rng["start"])
	if err != nil {
		return nil, false, err
	}
	end, err := dictOr(rng["end"])
	if err != nil {
		return nil, false, err
	}
	preStart, err := pyInt(dgetOr(start, "line", int64(0)))
	if err != nil {
		return nil, false, err
	}
	newStart, ok := shift(preStart)
	if !ok {
		return nil, false, nil
	}
	preEnd, err := pyInt(dgetOr(end, "line", preStart))
	if err != nil {
		return nil, false, err
	}
	newEnd, ok := shift(preEnd)
	if !ok {
		newEnd = newStart
	}
	startChar, err := pyInt(dgetOr(start, "character", int64(0)))
	if err != nil {
		return nil, false, err
	}
	endChar, err := pyInt(dgetOr(end, "character", int64(0)))
	if err != nil {
		return nil, false, err
	}
	out := make(map[string]any, len(diag)+1)
	for k, v := range diag {
		out[k] = v
	}
	out["range"] = map[string]any{
		"start": map[string]any{"line": newStart, "character": startChar},
		"end":   map[string]any{"line": newEnd, "character": endChar},
	}
	return out, true, nil
}

// ShiftBaseline remaps every baseline diagnostic, dropping deleted entries and non dicts
func ShiftBaseline(baseline []map[string]any, shift LineShift) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, d := range baseline {
		if d == nil {
			continue
		}
		s, ok, err := ShiftDiagnosticRange(d, shift)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
}
