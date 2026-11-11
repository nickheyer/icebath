package dashboard

import (
	"fmt"
	"math/big"
)

// Exactness pydantic core ranks a union member success by, lax below strict below exact
const (
	exactnessLax = iota
	exactnessStrict
	exactnessExact
)

// Pydantic smart mode union of scalar members, the most exact success winning and the first on ties
type unionType struct {
	members []FieldType
	labels  []string
}

// Smart mode union of int, str, float and bool members, any other member type panicking at declaration
func SmartUnion(members ...FieldType) FieldType {
	u := unionType{members: members}
	for _, m := range members {
		u.labels = append(u.labels, unionLabel(m))
	}
	return u
}

// Tag pydantic core gives a union member in the error location
func unionLabel(t FieldType) string {
	switch t.(type) {
	case intType:
		return "int"
	case strType:
		return "str"
	case floatType:
		return "float"
	case boolType:
		return "bool"
	}
	panic(fmt.Sprintf("SmartUnion member %T has no union label", t))
}

// Exactness of a member type validating an input it accepted
func unionExactness(t FieldType, v any) int {
	switch t.(type) {
	case intType:
		switch v.(type) {
		case int, int64, *big.Int:
			return exactnessExact
		}
	case strType:
		if _, ok := v.(string); ok {
			return exactnessExact
		}
	case floatType:
		switch v.(type) {
		case float64:
			return exactnessExact
		case int, int64, *big.Int:
			return exactnessStrict
		}
	case boolType:
		if _, ok := v.(bool); ok {
			return exactnessExact
		}
	}
	return exactnessLax
}

func (unionType) isSequence() bool { return false }

// Exact member success at once, else the best ranked success, else every member error under its tag
func (u unionType) Validate(v any) (any, []ErrorDetails) {
	var best any
	bestExactness, found := -1, false
	var errs []ErrorDetails
	for i, m := range u.members {
		out, e := m.Validate(v)
		if len(e) > 0 {
			if !found {
				errs = append(errs, prefixLoc([]any{u.labels[i]}, e)...)
			}
			continue
		}
		ex := unionExactness(m, v)
		if ex == exactnessExact {
			return out, nil
		}
		if !found || bestExactness < ex {
			best, bestExactness, found = out, ex, true
		}
	}
	if found {
		return best, nil
	}
	return nil, errs
}

// Field type whose before validator runs on the raw input, a ValueError becoming a value_error
type beforeValidatorType struct {
	fn    func(v any) (any, error)
	inner FieldType
}

// Inner type behind a pydantic field_validator in before mode
func BeforeValidator(fn func(v any) (any, error), inner FieldType) FieldType {
	return beforeValidatorType{fn: fn, inner: inner}
}

func (b beforeValidatorType) isSequence() bool { return b.inner.isSequence() }

// Inner validation of the validator output, a value_error carrying the ValueError text when it refuses
func (b beforeValidatorType) Validate(v any) (any, []ErrorDetails) {
	out, err := b.fn(v)
	if err != nil {
		return nil, []ErrorDetails{{Type: "value_error", Loc: []any{}, Msg: "Value error, " + err.Error(), Input: v,
			Ctx: jsonObj("error", err.Error())}}
	}
	return b.inner.Validate(out)
}
