package dashboard

import (
	"strconv"
	"unicode/utf8"
)

// String constrained by pydantic Field min_length and max_length, a nil bound absent
type strLenType struct {
	min, max *int64
}

// String whose character count stays within the inclusive bounds, a nil bound absent
func StrLen(min, max *int64) FieldType { return strLenType{min: min, max: max} }

func (strLenType) isSequence() bool { return false }

// Plural suffix pydantic core appends to a unit after a count
func expectedPlural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// The string, string_too_short before string_too_long for a character count outside the bounds
func (t strLenType) Validate(v any) (any, []ErrorDetails) {
	s, ok := v.(string)
	if !ok {
		return nil, valErr("string_type", "Input should be a valid string", v)
	}
	n := int64(utf8.RuneCountInString(s))
	if t.min != nil && n < *t.min {
		return nil, []ErrorDetails{{Type: "string_too_short", Loc: []any{},
			Msg:   "String should have at least " + strconv.FormatInt(*t.min, 10) + " character" + expectedPlural(*t.min),
			Input: v, Ctx: jsonObj("min_length", *t.min)}}
	}
	if t.max != nil && n > *t.max {
		return nil, []ErrorDetails{{Type: "string_too_long", Loc: []any{},
			Msg:   "String should have at most " + strconv.FormatInt(*t.max, 10) + " character" + expectedPlural(*t.max),
			Input: v, Ctx: jsonObj("max_length", *t.max)}}
	}
	return s, nil
}

// Strict integer constrained to the inclusive bounds, a nil bound absent
func StrictIntRange(ge, le *int64) FieldType { return intType{strict: true, ge: ge, le: le} }
