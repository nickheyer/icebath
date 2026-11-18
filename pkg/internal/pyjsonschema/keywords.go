package pyjsonschema

import (
	"fmt"
	"iter"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Keyword functions of jsonschema _keywords.py and _legacy_keywords.py

type errSeq = iter.Seq[*vError]

func yieldFrom(errs errSeq, yield func(*vError) bool) bool {
	for e := range errs {
		if !yield(e) {
			return false
		}
	}
	return true
}

// dict.items of a value, the AttributeError a non dict raises
func pyItems(v any) [][2]any {
	d, ok := v.(*pyjson.Object)
	if !ok {
		raise("AttributeError", "'"+typeName(v)+"' object has no attribute 'items'")
	}
	out := make([][2]any, len(d.Keys))
	for i, k := range d.Keys {
		out[i] = [2]any{k, d.Vals[k]}
	}
	return out
}

// ensure_list, a frame of its own
func ensureList(st *state, v any) []any {
	st.touch(1)
	if s, ok := v.(string); ok {
		return []any{s}
	}
	return pyIter(v)
}

// Rich comparison of two values with the TypeError an unordered pair raises
func compare(a, b any, op string) bool {
	if !numeric(a) || !numeric(b) {
		raise("TypeError", fmt.Sprintf("'%s' not supported between instances of '%s' and '%s'", op, typeName(a), typeName(b)))
	}
	c, ok := numCmp(a, b)
	if !ok {
		return false
	}
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}

func truthy(v any) bool { return pyrepr.Truthy(v) }

// extras_msg, its frame and the join generator expression under it
func (v *validator) extrasMsg(extras []any) string {
	v.st.touch(2)
	verb := "were"
	if len(extras) == 1 {
		verb = "was"
	}
	parts := make([]string, len(extras))
	for i, e := range extras {
		parts[i] = repr(e)
	}
	return strings.Join(parts, ", ") + " " + verb
}

func kwPatternProperties(v *validator, value, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, pp := range pyItems(value) {
			pattern := pp[0].(string)
			for _, k := range inst.Keys {
				if v.st.search(pattern, k) {
					if !yieldFrom(v.descend(inst.Vals[k], pp[1], k, pattern, nil), yield) {
						return
					}
				}
			}
		}
	}
}

func kwPropertyNames(v *validator, value, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		for _, k := range instance.(*pyjson.Object).Keys {
			if !yieldFrom(v.descend(k, value, nil, nil, nil), yield) {
				return
			}
		}
	}
}

// find_additional_properties, instance keys neither properties nor the joined patternProperties cover
func findAdditionalProperties(v *validator, instance *pyjson.Object, schema *pyjson.Object) []string {
	defer v.st.call()()
	properties, ok := schema.Vals["properties"]
	if !ok {
		properties = pyjson.NewObject()
	}
	var patternNames []string
	if pp, ok := schema.Vals["patternProperties"]; ok {
		for _, p := range pyIter(pp) {
			s, isStr := p.(string)
			if !isStr {
				raise("TypeError", "sequence item 0: expected str instance, "+typeName(p)+" found")
			}
			patternNames = append(patternNames, s)
		}
	}
	patterns := strings.Join(patternNames, "|")
	var out []string
	for _, k := range instance.Keys {
		if !pyIn(k, properties) {
			if patterns != "" && v.st.search(patterns, k) {
				continue
			}
			out = append(out, k)
		}
	}
	return out
}

func kwAdditionalProperties(v *validator, aP, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		extras := findAdditionalProperties(v, inst, schema)
		if v.isType(aP, "object") {
			for _, extra := range extras {
				if !yieldFrom(v.descend(inst.Vals[extra], aP, extra, nil, nil), yield) {
					return
				}
			}
			return
		}
		if truthy(aP) || len(extras) == 0 {
			return
		}
		sorted := append([]string(nil), extras...)
		sort.Strings(sorted)
		if pp, ok := schema.Vals["patternProperties"]; ok {
			verb := "do"
			if len(extras) == 1 {
				verb = "does"
			}
			v.st.touch(1)
			joined := make([]string, len(sorted))
			for i, e := range sorted {
				joined[i] = repr(e)
			}
			var names []string
			for _, p := range pyIter(pp) {
				names = append(names, p.(string))
			}
			sort.Strings(names)
			v.st.touch(1)
			patterns := make([]string, len(names))
			for i, p := range names {
				patterns[i] = repr(p)
			}
			yield(v.errorf(strings.Join(joined, ", ") + " " + verb + " not match any of the regexes: " + strings.Join(patterns, ", ")))
			return
		}
		items := make([]any, len(sorted))
		for i, e := range sorted {
			items[i] = e
		}
		yield(v.errorf("Additional properties are not allowed (" + v.extrasMsg(items) + " unexpected)"))
	}
}

func kwItems(v *validator, items, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		list := instance.([]any)
		prefixItems, ok := schema.Vals["prefixItems"]
		if !ok {
			prefixItems = []any{}
		}
		prefix := pyLen(prefixItems)
		total := len(list)
		extra := total - prefix
		if extra <= 0 {
			return
		}
		if items == false {
			var rest any = list[prefix:]
			if extra == 1 {
				rest = list[prefix]
			}
			word := "items"
			if prefix == 1 {
				word = "item"
			}
			yield(v.errorf(fmt.Sprintf("Expected at most %d %s but found %d extra: %s", prefix, word, extra, repr(rest))))
			return
		}
		for index := prefix; index < total; index++ {
			if !yieldFrom(v.descend(list[index], items, index, nil, nil), yield) {
				return
			}
		}
	}
}

func kwConst(v *validator, value, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !equal(v.st, instance, value) {
			yield(v.errorf(repr(value) + " was expected"))
		}
	}
}

func kwContains(v *validator, contains, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		list := instance.([]any)
		matches := int64(0)
		minContains, ok := schema.Vals["minContains"]
		if !ok {
			minContains = int64(1)
		}
		maxContains, ok := schema.Vals["maxContains"]
		if !ok {
			maxContains = int64(len(list))
		}
		cv := v.evolve(contains, nil)
		for _, each := range list {
			if cv.isValid(each) {
				matches++
				if compare(matches, maxContains, ">") {
					yield(newFullErrorPartial("Too many items match the given schema (expected at most "+pyStr(maxContains)+")", "maxContains", maxContains))
					return
				}
			}
		}
		if compare(matches, minContains, "<") {
			if matches == 0 {
				yield(v.errorf(repr(instance) + " does not contain items matching the given schema"))
				return
			}
			yield(newFullErrorPartial(fmt.Sprintf("Too few items match the given schema (expected at least %s but only %d matched)", pyStr(minContains), matches),
				"minContains", minContains))
		}
	}
}

// Error with validator and validator_value preset, instance and schema left for iter_errors
func newFullErrorPartial(message string, validator, value any) *vError {
	return &vError{message: message, validator: validator, validatorValue: value, set: [4]bool{true, true, false, false}}
}

func kwExclusiveMinimum(v *validator, minimum, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "number") && compare(instance, minimum, "<=") {
			yield(v.errorf(repr(instance) + " is less than or equal to the minimum of " + repr(minimum)))
		}
	}
}

func kwExclusiveMaximum(v *validator, maximum, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "number") && compare(instance, maximum, ">=") {
			yield(v.errorf(repr(instance) + " is greater than or equal to the maximum of " + repr(maximum)))
		}
	}
}

func kwMinimum(v *validator, minimum, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "number") && compare(instance, minimum, "<") {
			yield(v.errorf(repr(instance) + " is less than the minimum of " + repr(minimum)))
		}
	}
}

func kwMaximum(v *validator, maximum, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "number") && compare(instance, maximum, ">") {
			yield(v.errorf(repr(instance) + " is greater than the maximum of " + repr(maximum)))
		}
	}
}

// Float value of an int or float operand of true division, OverflowError for an int beyond float range
func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	n := bigOf(v)
	if n == nil {
		raise("TypeError", "must be real number, not "+typeName(v))
	}
	f, _ := new(big.Float).SetInt(n).Float64()
	if math.IsInf(f, 0) {
		raise("OverflowError", "int too large to convert to float")
	}
	return f
}

// Exact rational of an int or float the way fractions.Fraction builds one
func toRat(v any) *big.Rat {
	if f, ok := v.(float64); ok {
		if math.IsInf(f, 0) {
			raise("OverflowError", "cannot convert Infinity to integer ratio")
		}
		if math.IsNaN(f) {
			raise("ValueError", "cannot convert NaN to integer ratio")
		}
		return new(big.Rat).SetFloat64(f)
	}
	return new(big.Rat).SetInt(bigOf(v))
}

func kwMultipleOf(v *validator, dB, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "number") {
			return
		}
		failed := false
		if f, isFloat := dB.(float64); isFloat {
			x := toFloat(instance)
			if f == 0 {
				raise("ZeroDivisionError", "division by zero")
			}
			q := x / f
			switch {
			case math.IsNaN(q):
				raise("ValueError", "cannot convert float NaN to integer")
			case math.IsInf(q, 0):
				v.st.touch(3)
				failed = !new(big.Rat).Quo(toRat(instance), toRat(dB)).IsInt()
			default:
				failed = q != math.Trunc(q)
			}
		} else {
			failed = modNonzero(instance, dB)
		}
		if failed {
			yield(v.errorf(repr(instance) + " is not a multiple of " + pyStr(dB)))
		}
	}
}

// Truth of instance % dB for an int divisor
func modNonzero(instance, dB any) bool {
	d := bigOf(dB)
	if d == nil {
		raise("TypeError", "unsupported operand type(s) for %: '"+typeName(instance)+"' and '"+typeName(dB)+"'")
	}
	if f, ok := instance.(float64); ok {
		y := toFloat(dB)
		if y == 0 {
			raise("ZeroDivisionError", "division by zero")
		}
		r := math.Mod(f, y)
		return r != 0 || math.IsNaN(r)
	}
	if d.Sign() == 0 {
		raise("ZeroDivisionError", "division by zero")
	}
	return new(big.Int).Rem(bigOf(instance), d).Sign() != 0
}

func kwMinItems(v *validator, mI, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "array") && compare(int64(len(instance.([]any))), mI, "<") {
			msg := "is too short"
			if pyEq(mI, int64(1)) {
				msg = "should be non-empty"
			}
			yield(v.errorf(repr(instance) + " " + msg))
		}
	}
}

func kwMaxItems(v *validator, mI, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "array") && compare(int64(len(instance.([]any))), mI, ">") {
			msg := "is too long"
			if pyEq(mI, int64(0)) {
				msg = "is expected to be empty"
			}
			yield(v.errorf(repr(instance) + " " + msg))
		}
	}
}

func kwUniqueItems(v *validator, uI, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if truthy(uI) && v.isType(instance, "array") && !uniq(v.st, instance.([]any)) {
			yield(v.errorf(repr(instance) + " has non-unique elements"))
		}
	}
}

func kwPattern(v *validator, patrn, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "string") {
			return
		}
		p, ok := patrn.(string)
		if !ok {
			raise("TypeError", "first argument must be string or compiled pattern")
		}
		if !v.st.search(p, instance.(string)) {
			yield(v.errorf(repr(instance) + " does not match " + repr(patrn)))
		}
	}
}

// format, checked only under a format checker whose registered formats are those of the [all] install
func kwFormat(v *validator, format, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.formats {
			return
		}
		if !v.formatConforms(format, instance) {
			yield(v.errorf(repr(instance) + " is not a " + repr(format)))
		}
	}
}

// FormatChecker.check under the format checker of the [all] install, regex the one metaschema format it registers
func (v *validator) formatConforms(format, instance any) bool {
	defer v.st.call()()
	if format != "regex" {
		return true
	}
	pattern, ok := instance.(string)
	if !ok {
		v.st.touch(1)
		return true
	}
	conforms, depth := regexCompiles(v.st, pattern)
	v.st.touch(1 + depth)
	if !conforms {
		v.st.touch(1)
	}
	return conforms
}

// is_regex, false for the re.error re.compile raises, with the frames re.compile reached
func regexCompiles(st *state, pattern string) (ok bool, depth int) {
	defer func() {
		if r := recover(); r != nil {
			if e, isExc := r.(*Exception); isExc && e.Type == "PatternError" {
				ok, depth = false, patternErrorDepth(pattern, e)
				return
			}
			panic(r)
		}
	}()
	st.compile(pattern)
	return true, 2
}

// Frames re.compile reaches parsing a pattern up to its error, two per group open at the deepest point
func patternErrorDepth(pattern string, e *Exception) int {
	end := len(pattern)
	if i := strings.LastIndex(e.Msg, " at position "); i >= 0 {
		pos, err := strconv.Atoi(strings.Fields(e.Msg[i+len(" at position "):])[0])
		if err == nil {
			rs := []rune(pattern)
			end = len(string(rs[:min(pos+1, len(rs))]))
		}
	}
	nest, deepest := 0, 0
	escaped, inClass, emptyAtDeepest := false, false, false
	for _, r := range pattern[:end] {
		switch {
		case escaped:
			escaped = false
			emptyAtDeepest = false
		case r == '\\':
			escaped = true
		case inClass:
			if r == ']' {
				inClass = false
			}
		case r == '[':
			inClass = true
			emptyAtDeepest = false
		case r == '(':
			nest++
			if nest >= deepest {
				deepest, emptyAtDeepest = nest, true
			}
		case r == ')':
			nest--
		default:
			emptyAtDeepest = false
		}
	}
	if deepest > 0 && emptyAtDeepest {
		return 7 + 2*deepest
	}
	return 8 + 2*deepest
}

func kwMinLength(v *validator, mL, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "string") && compare(int64(pyLen(instance)), mL, "<") {
			msg := "is too short"
			if pyEq(mL, int64(1)) {
				msg = "should be non-empty"
			}
			yield(v.errorf(repr(instance) + " " + msg))
		}
	}
}

func kwMaxLength(v *validator, mL, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "string") && compare(int64(pyLen(instance)), mL, ">") {
			msg := "is too long"
			if pyEq(mL, int64(0)) {
				msg = "is expected to be empty"
			}
			yield(v.errorf(repr(instance) + " " + msg))
		}
	}
}

func kwDependentRequired(v *validator, value, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, kv := range pyItems(value) {
			if !dictHas(inst, kv[0]) {
				continue
			}
			for _, each := range pyIter(kv[1]) {
				if !dictHas(inst, each) {
					if !yield(v.errorf(repr(each) + " is a dependency of " + repr(kv[0]))) {
						return
					}
				}
			}
		}
	}
}

func kwDependentSchemas(v *validator, value, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, kv := range pyItems(value) {
			if !dictHas(inst, kv[0]) {
				continue
			}
			if !yieldFrom(v.descend(instance, kv[1], nil, kv[0], nil), yield) {
				return
			}
		}
	}
}

func kwEnum(v *validator, enums, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		matched := func() bool {
			defer v.st.call()()
			for _, each := range pyIter(enums) {
				if equal(v.st, each, instance) {
					return true
				}
			}
			return false
		}()
		if matched {
			return
		}
		yield(v.errorf(repr(instance) + " is not one of " + repr(enums)))
	}
}

func kwRef(v *validator, ref, instance any, _ *pyjson.Object) errSeq {
	return v.validateReference(ref, instance)
}

func kwDynamicRef(v *validator, ref, instance any, _ *pyjson.Object) errSeq {
	return v.validateReference(ref, instance)
}

func kwType(v *validator, types, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		list := ensureList(v.st, types)
		matched := func() bool {
			defer v.st.call()()
			for _, t := range list {
				if v.isType(instance, t) {
					return true
				}
			}
			return false
		}()
		if matched {
			return
		}
		v.st.touch(1)
		reprs := make([]string, len(list))
		for i, t := range list {
			reprs[i] = repr(t)
		}
		yield(v.errorf(repr(instance) + " is not of type " + strings.Join(reprs, ", ")))
	}
}

func kwProperties(v *validator, properties, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, kv := range pyItems(properties) {
			k := kv[0].(string)
			if val, ok := inst.Vals[k]; ok {
				if !yieldFrom(v.descend(val, kv[1], k, k, nil), yield) {
					return
				}
			}
		}
	}
}

func kwRequired(v *validator, required, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, property := range pyIter(required) {
			if !dictHas(inst, property) {
				if !yield(v.errorf(repr(property) + " is a required property")) {
					return
				}
			}
		}
	}
}

func kwMinProperties(v *validator, mP, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(instance, "object") && compare(int64(pyLen(instance)), mP, "<") {
			msg := "does not have enough properties"
			if pyEq(mP, int64(1)) {
				msg = "should be non-empty"
			}
			yield(v.errorf(repr(instance) + " " + msg))
		}
	}
}

func kwMaxProperties(v *validator, mP, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		if v.isType(instance, "object") && compare(int64(pyLen(instance)), mP, ">") {
			msg := "has too many properties"
			if pyEq(mP, int64(0)) {
				msg = "is expected to be empty"
			}
			yield(v.errorf(repr(instance) + " " + msg))
		}
	}
}

func kwAllOf(v *validator, allOf, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		for index, subschema := range pyIter(allOf) {
			if !yieldFrom(v.descend(instance, subschema, nil, index, nil), yield) {
				return
			}
		}
	}
}

func collect(errs errSeq) []*vError {
	var out []*vError
	for e := range errs {
		out = append(out, e)
	}
	return out
}

func kwAnyOf(v *validator, anyOf, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		var all []*vError
		for index, subschema := range pyIter(anyOf) {
			errs := collect(v.descend(instance, subschema, nil, index, nil))
			if len(errs) == 0 {
				return
			}
			all = append(all, errs...)
		}
		yield(withContext(v.errorf(repr(instance)+" is not valid under any of the given schemas"), all))
	}
}

func kwOneOf(v *validator, oneOf, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		subschemas := pyIter(oneOf)
		var all []*vError
		var firstValid any
		found := -1
		for index, subschema := range subschemas {
			errs := collect(v.descend(instance, subschema, nil, index, nil))
			if len(errs) == 0 {
				firstValid, found = subschema, index
				break
			}
			all = append(all, errs...)
		}
		if found < 0 {
			if !yield(withContext(v.errorf(repr(instance)+" is not valid under any of the given schemas"), all)) {
				return
			}
			return
		}
		var moreValid []any
		for _, each := range subschemas[found+1:] {
			if v.evolve(each, nil).isValid(instance) {
				moreValid = append(moreValid, each)
			}
		}
		if len(moreValid) > 0 {
			moreValid = append(moreValid, firstValid)
			v.st.touch(1)
			reprs := make([]string, len(moreValid))
			for i, s := range moreValid {
				reprs[i] = repr(s)
			}
			yield(v.errorf(repr(instance) + " is valid under each of " + strings.Join(reprs, ", ")))
		}
	}
}

func kwNot(v *validator, notSchema, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.evolve(notSchema, nil).isValid(instance) {
			yield(v.errorf(repr(instance) + " should not be valid under " + repr(notSchema)))
		}
	}
}

func kwIf(v *validator, ifSchema, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.evolve(ifSchema, nil).isValid(instance) {
			if then, ok := schema.Vals["then"]; ok {
				yieldFrom(v.descend(instance, then, nil, "then", nil), yield)
			}
		} else if els, ok := schema.Vals["else"]; ok {
			yieldFrom(v.descend(instance, els, nil, "else", nil), yield)
		}
	}
}

func kwUnevaluatedItems(v *validator, _, instance any, schema *pyjson.Object) errSeq {
	return unevaluatedItemsWith(v, instance, schema, findEvaluatedItemIndexes)
}

func kwUnevaluatedItemsDraft2019(v *validator, _, instance any, schema *pyjson.Object) errSeq {
	return unevaluatedItemsWith(v, instance, schema, findEvaluatedItemIndexesDraft2019)
}

func unevaluatedItemsWith(v *validator, instance any, schema *pyjson.Object, find func(*validator, any, any) []int) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		evaluated := find(v, instance, schema)
		seen := map[int]bool{}
		for _, i := range evaluated {
			seen[i] = true
		}
		var unevaluated []any
		for i, item := range instance.([]any) {
			if !seen[i] {
				unevaluated = append(unevaluated, item)
			}
		}
		if len(unevaluated) > 0 {
			yield(v.errorf("Unevaluated items are not allowed (" + v.extrasMsg(unevaluated) + " unexpected)"))
		}
	}
}

func kwUnevaluatedProperties(v *validator, uP, instance any, schema *pyjson.Object) errSeq {
	return unevaluatedPropertiesWith(v, uP, instance, schema, findEvaluatedPropertyKeys)
}

func kwUnevaluatedPropertiesDraft2019(v *validator, uP, instance any, schema *pyjson.Object) errSeq {
	return unevaluatedPropertiesWith(v, uP, instance, schema, findEvaluatedPropertyKeysDraft2019)
}

func unevaluatedPropertiesWith(v *validator, uP, instance any, schema *pyjson.Object, find func(*validator, any, any) []string) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		evaluated := map[string]bool{}
		for _, k := range find(v, instance, schema) {
			evaluated[k] = true
		}
		var unevaluated []any
		for _, k := range inst.Keys {
			if evaluated[k] {
				continue
			}
			for range v.descend(inst.Vals[k], uP, k, k, nil) {
				unevaluated = append(unevaluated, k)
			}
		}
		if len(unevaluated) == 0 {
			return
		}
		if uP == false {
			sorted := append([]any(nil), unevaluated...)
			sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].(string) < sorted[j].(string) })
			yield(v.errorf("Unevaluated properties are not allowed (" + v.extrasMsg(sorted) + " unexpected)"))
			return
		}
		yield(v.errorf("Unevaluated properties are not valid under the given schema (" + v.extrasMsg(unevaluated) + " unevaluated and invalid)"))
	}
}

func kwPrefixItems(v *validator, prefixItems, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		list := instance.([]any)
		subs := pyIter(prefixItems)
		for index := 0; index < len(list) && index < len(subs); index++ {
			if !yieldFrom(v.descend(list[index], subs[index], index, index, nil), yield) {
				return
			}
		}
	}
}

func kwDependenciesDraft3(v *validator, dependencies, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, kv := range pyItems(dependencies) {
			property, dependency := kv[0], kv[1]
			if !dictHas(inst, property) {
				continue
			}
			switch {
			case v.isType(dependency, "object"):
				if !yieldFrom(v.descend(instance, dependency, nil, property, nil), yield) {
					return
				}
			case v.isType(dependency, "string"):
				if !dictHas(inst, dependency) {
					if !yield(v.errorf(repr(dependency) + " is a dependency of " + repr(property))) {
						return
					}
				}
			default:
				for _, each := range pyIter(dependency) {
					if !dictHas(inst, each) {
						if !yield(v.errorf(repr(each) + " is a dependency of " + repr(property))) {
							return
						}
					}
				}
			}
		}
	}
}

func kwDependenciesDraft4(v *validator, dependencies, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, kv := range pyItems(dependencies) {
			property, dependency := kv[0], kv[1]
			if !dictHas(inst, property) {
				continue
			}
			if v.isType(dependency, "array") {
				for _, each := range dependency.([]any) {
					if !dictHas(inst, each) {
						if !yield(v.errorf(repr(each) + " is a dependency of " + repr(property))) {
							return
						}
					}
				}
				continue
			}
			if !yieldFrom(v.descend(instance, dependency, nil, property, nil), yield) {
				return
			}
		}
	}
}

func kwDisallowDraft3(v *validator, disallow, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		for _, disallowed := range ensureList(v.st, disallow) {
			if v.evolve(pyjson.ObjectOf("type", []any{disallowed}), nil).isValid(instance) {
				if !yield(v.errorf(repr(disallowed) + " is disallowed for " + repr(instance))) {
					return
				}
			}
		}
	}
}

func kwExtendsDraft3(v *validator, extends, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if v.isType(extends, "object") {
			yieldFrom(v.descend(instance, extends, nil, nil, nil), yield)
			return
		}
		for index, subschema := range pyIter(extends) {
			if !yieldFrom(v.descend(instance, subschema, nil, index, nil), yield) {
				return
			}
		}
	}
}

func kwItemsDraft3Draft4(v *validator, items, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		list := instance.([]any)
		if v.isType(items, "object") {
			for index, item := range list {
				if !yieldFrom(v.descend(item, items, index, nil, nil), yield) {
					return
				}
			}
			return
		}
		subs := pyIter(items)
		for index := 0; index < len(list) && index < len(subs); index++ {
			if !yieldFrom(v.descend(list[index], subs[index], index, index, nil), yield) {
				return
			}
		}
	}
}

func kwAdditionalItems(v *validator, aI, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		items, ok := schema.Vals["items"]
		if !ok {
			items = pyjson.NewObject()
		}
		if !v.isType(instance, "array") || v.isType(items, "object") {
			return
		}
		list := instance.([]any)
		lenItems := 0
		if itemsVal, ok := schema.Vals["items"]; ok {
			lenItems = pyLen(itemsVal)
		}
		if v.isType(aI, "object") {
			for index := lenItems; index < len(list); index++ {
				if !yieldFrom(v.descend(list[index], aI, index, nil, nil), yield) {
					return
				}
			}
			return
		}
		if !truthy(aI) && len(list) > lenItems {
			yield(v.errorf("Additional items are not allowed (" + v.extrasMsg(list[lenItems:]) + " unexpected)"))
		}
	}
}

func kwItemsDraft6To2019(v *validator, items, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		list := instance.([]any)
		if v.isType(items, "array") {
			subs := items.([]any)
			for index := 0; index < len(list) && index < len(subs); index++ {
				if !yieldFrom(v.descend(list[index], subs[index], index, index, nil), yield) {
					return
				}
			}
			return
		}
		for index, item := range list {
			if !yieldFrom(v.descend(item, items, index, nil, nil), yield) {
				return
			}
		}
	}
}

func kwMinimumDraft3Draft4(v *validator, minimum, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "number") {
			return
		}
		failed, cmp := compare(instance, minimum, "<"), "less than"
		if truthy(schema.Vals["exclusiveMinimum"]) {
			failed, cmp = compare(instance, minimum, "<="), "less than or equal to"
		}
		if failed {
			yield(v.errorf(repr(instance) + " is " + cmp + " the minimum of " + repr(minimum)))
		}
	}
}

func kwMaximumDraft3Draft4(v *validator, maximum, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "number") {
			return
		}
		failed, cmp := compare(instance, maximum, ">"), "greater than"
		if truthy(schema.Vals["exclusiveMaximum"]) {
			failed, cmp = compare(instance, maximum, ">="), "greater than or equal to"
		}
		if failed {
			yield(v.errorf(repr(instance) + " is " + cmp + " the maximum of " + repr(maximum)))
		}
	}
}

func kwPropertiesDraft3(v *validator, properties, instance any, schema *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "object") {
			return
		}
		inst := instance.(*pyjson.Object)
		for _, kv := range pyItems(properties) {
			property := kv[0].(string)
			subschema := kv[1]
			if val, ok := inst.Vals[property]; ok {
				if !yieldFrom(v.descend(val, subschema, property, property, nil), yield) {
					return
				}
				continue
			}
			req := pyGet(subschema, "required")
			if req == nil || !truthy(req) {
				continue
			}
			e := v.errorf(repr(property) + " is a required property")
			v.st.touch(1)
			e.setDetails("required", pySubscript(subschema, "required"), instance, schema, nil)
			e.path = prepend(e.path, property)
			e.schemaPath = append(e.schemaPath, property, "required")
			if !yield(e) {
				return
			}
		}
	}
}

func kwTypeDraft3(v *validator, types, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		list := ensureList(v.st, types)
		var all []*vError
		for index, t := range list {
			if v.isType(t, "object") {
				errs := collect(v.descend(instance, t, nil, index, nil))
				if len(errs) == 0 {
					return
				}
				all = append(all, errs...)
			} else if v.isType(instance, t) {
				return
			}
		}
		reprs := make([]string, len(list))
		for i, t := range list {
			if d, ok := t.(*pyjson.Object); ok {
				if name, ok := d.Vals["name"]; ok {
					reprs[i] = repr(name)
					continue
				}
			}
			reprs[i] = repr(t)
		}
		yield(withContext(v.errorf(repr(instance)+" is not of type "+strings.Join(reprs, ", ")), all))
	}
}

func kwContainsDraft6Draft7(v *validator, contains, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		if !v.isType(instance, "array") {
			return
		}
		matched := func() bool {
			defer v.st.call()()
			for _, element := range instance.([]any) {
				if v.evolve(contains, nil).isValid(element) {
					return true
				}
			}
			return false
		}()
		if matched {
			return
		}
		yield(v.errorf("None of " + repr(instance) + " are valid under the given schema"))
	}
}

func kwRecursiveRef(v *validator, _, instance any, _ *pyjson.Object) errSeq {
	return func(yield func(*vError) bool) {
		contents, res := lookupRecursiveRef(v.resolver)
		yieldFrom(v.descend(instance, contents, nil, nil, res), yield)
	}
}

func rangeInts(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// _utils.find_evaluated_item_indexes_by_schema, the indexes a draft 2020 schema evaluates
func findEvaluatedItemIndexes(v *validator, instance, schema any) []int {
	defer v.st.call()()
	if v.isType(schema, "boolean") {
		return nil
	}
	var out []int
	if pyIn("items", schema) {
		return rangeInts(pyLen(instance))
	}
	for _, key := range []string{"$ref", "$dynamicRef"} {
		if ref := pyGet(schema, key); ref != nil {
			contents, res := v.lookup(ref)
			out = append(out, findEvaluatedItemIndexes(v.evolve(contents, res), instance, contents)...)
		}
	}
	if pyIn("prefixItems", schema) {
		out = append(out, rangeInts(pyLen(pySubscript(schema, "prefixItems")))...)
	}
	out = append(out, evaluatedByIf(v, instance, schema, findEvaluatedItemIndexes)...)
	out = append(out, evaluatedByContains(v, instance, schema)...)
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		if pyIn(keyword, schema) {
			for _, sub := range pyIter(pySubscript(schema, keyword)) {
				if isValidSeq(v.descend(instance, sub, nil, nil, nil)) {
					out = append(out, findEvaluatedItemIndexes(v, instance, sub)...)
				}
			}
		}
	}
	return out
}

// _legacy_keywords.find_evaluated_item_indexes_by_schema, the indexes a draft 2019 schema evaluates
func findEvaluatedItemIndexesDraft2019(v *validator, instance, schema any) []int {
	defer v.st.call()()
	if v.isType(schema, "boolean") {
		return nil
	}
	var out []int
	if ref := pyGet(schema, "$ref"); ref != nil {
		contents, res := v.lookup(ref)
		out = append(out, findEvaluatedItemIndexesDraft2019(v.evolve(contents, res), instance, contents)...)
	}
	if pyIn("$recursiveRef", schema) {
		contents, res := lookupRecursiveRef(v.resolver)
		out = append(out, findEvaluatedItemIndexesDraft2019(v.evolve(contents, res), instance, contents)...)
	}
	if pyIn("items", schema) {
		if pyIn("additionalItems", schema) {
			return rangeInts(pyLen(instance))
		}
		items := pySubscript(schema, "items")
		if v.isType(items, "object") {
			return rangeInts(pyLen(instance))
		}
		out = append(out, rangeInts(pyLen(items))...)
	}
	out = append(out, evaluatedByIf(v, instance, schema, findEvaluatedItemIndexesDraft2019)...)
	out = append(out, evaluatedByContains(v, instance, schema)...)
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		if pyIn(keyword, schema) {
			for _, sub := range pyIter(pySubscript(schema, keyword)) {
				if isValidSeq(v.descend(instance, sub, nil, nil, nil)) {
					out = append(out, findEvaluatedItemIndexesDraft2019(v, instance, sub)...)
				}
			}
		}
	}
	return out
}

func evaluatedByIf[T any](v *validator, instance, schema any, find func(*validator, any, any) []T) []T {
	if !pyIn("if", schema) {
		return nil
	}
	ifSchema := pySubscript(schema, "if")
	var out []T
	if v.evolve(ifSchema, nil).isValid(instance) {
		out = append(out, find(v, instance, ifSchema)...)
		if pyIn("then", schema) {
			out = append(out, find(v, instance, pySubscript(schema, "then"))...)
		}
	} else if pyIn("else", schema) {
		out = append(out, find(v, instance, pySubscript(schema, "else"))...)
	}
	return out
}

func evaluatedByContains(v *validator, instance, schema any) []int {
	var out []int
	for _, keyword := range []string{"contains", "unevaluatedItems"} {
		if pyIn(keyword, schema) {
			for k, item := range pyIter(instance) {
				if v.evolve(pySubscript(schema, keyword), nil).isValid(item) {
					out = append(out, k)
				}
			}
		}
	}
	return out
}

// _utils.find_evaluated_property_keys_by_schema, the keys a draft 2020 schema evaluates
func findEvaluatedPropertyKeys(v *validator, instance, schema any) []string {
	defer v.st.call()()
	if v.isType(schema, "boolean") {
		return nil
	}
	inst := instance.(*pyjson.Object)
	var out []string
	for _, key := range []string{"$ref", "$dynamicRef"} {
		if ref := pyGet(schema, key); ref != nil {
			contents, res := v.lookup(ref)
			out = append(out, findEvaluatedPropertyKeys(v.evolve(contents, res), instance, contents)...)
		}
	}
	if properties := pyGet(schema, "properties"); v.isType(properties, "object") {
		for _, k := range properties.(*pyjson.Object).Keys {
			if _, ok := inst.Vals[k]; ok {
				out = append(out, k)
			}
		}
	}
	for _, keyword := range []string{"additionalProperties", "unevaluatedProperties"} {
		sub := pyGet(schema, keyword)
		if sub == nil {
			continue
		}
		func() {
			defer v.st.call()()
			for _, k := range inst.Keys {
				if v.isValidFn(v.descend(inst.Vals[k], sub, nil, nil, nil)) {
					out = append(out, k)
				}
			}
		}()
	}
	out = append(out, evaluatedByPatterns(v, inst, schema)...)
	out = append(out, evaluatedByDependentSchemas(v, inst, schema, findEvaluatedPropertyKeys)...)
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		subs, ok := asDict(schema).Vals[keyword]
		if !ok {
			subs = []any{}
		}
		for _, sub := range pyIter(subs) {
			if !v.isValidFn(v.descend(instance, sub, nil, nil, nil)) {
				continue
			}
			out = append(out, findEvaluatedPropertyKeys(v, instance, sub)...)
		}
	}
	out = append(out, evaluatedByIf(v, instance, schema, findEvaluatedPropertyKeys)...)
	return out
}

// _legacy_keywords.find_evaluated_property_keys_by_schema, the keys a draft 2019 schema evaluates
func findEvaluatedPropertyKeysDraft2019(v *validator, instance, schema any) []string {
	defer v.st.call()()
	if v.isType(schema, "boolean") {
		return nil
	}
	inst := instance.(*pyjson.Object)
	var out []string
	if ref := pyGet(schema, "$ref"); ref != nil {
		contents, res := v.lookup(ref)
		out = append(out, findEvaluatedPropertyKeysDraft2019(v.evolve(contents, res), instance, contents)...)
	}
	if pyIn("$recursiveRef", schema) {
		contents, res := lookupRecursiveRef(v.resolver)
		out = append(out, findEvaluatedPropertyKeysDraft2019(v.evolve(contents, res), instance, contents)...)
	}
	for _, keyword := range []string{"properties", "additionalProperties", "unevaluatedProperties"} {
		if !pyIn(keyword, schema) {
			continue
		}
		value := pySubscript(schema, keyword)
		if v.isType(value, "boolean") && truthy(value) {
			out = append(out, inst.Keys...)
		} else if v.isType(value, "object") {
			for _, k := range value.(*pyjson.Object).Keys {
				if _, ok := inst.Vals[k]; ok {
					out = append(out, k)
				}
			}
		}
	}
	out = append(out, evaluatedByPatterns(v, inst, schema)...)
	out = append(out, evaluatedByDependentSchemas(v, inst, schema, findEvaluatedPropertyKeysDraft2019)...)
	for _, keyword := range []string{"allOf", "oneOf", "anyOf"} {
		if pyIn(keyword, schema) {
			for _, sub := range pyIter(pySubscript(schema, keyword)) {
				if isValidSeq(v.descend(instance, sub, nil, nil, nil)) {
					out = append(out, findEvaluatedPropertyKeysDraft2019(v, instance, sub)...)
				}
			}
		}
	}
	out = append(out, evaluatedByIf(v, instance, schema, findEvaluatedPropertyKeysDraft2019)...)
	return out
}

func evaluatedByPatterns(v *validator, inst *pyjson.Object, schema any) []string {
	if !pyIn("patternProperties", schema) {
		return nil
	}
	patterns := pyIter(pySubscript(schema, "patternProperties"))
	var out []string
	for _, k := range inst.Keys {
		for _, p := range patterns {
			ps, ok := p.(string)
			if !ok {
				raise("TypeError", "first argument must be string or compiled pattern")
			}
			if v.st.search(ps, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

func evaluatedByDependentSchemas(v *validator, inst *pyjson.Object, schema any, find func(*validator, any, any) []string) []string {
	if !pyIn("dependentSchemas", schema) {
		return nil
	}
	var out []string
	for _, kv := range pyItems(pySubscript(schema, "dependentSchemas")) {
		if !dictHas(inst, kv[0]) {
			continue
		}
		out = append(out, find(v, inst, kv[1])...)
	}
	return out
}
