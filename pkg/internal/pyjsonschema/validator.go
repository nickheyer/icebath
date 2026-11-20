package pyjsonschema

import (
	"errors"
	"iter"
	"math"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Port of jsonschema 4.26.0 validators.create and the six draft validator classes it builds

type keywordFn func(v *validator, value, instance any, schema *pyjson.Object) iter.Seq[*vError]

// One validator class, its keywords, type checker, format checker and referencing specification
type draft struct {
	name              string
	metaURI           string
	keywords          map[string]keywordFn
	types             map[string]func(any) bool
	ignoreRefSiblings bool
	spec              *specification
}

func isAnyType(any) bool     { return true }
func isArrayType(v any) bool { return isList(v) }
func isBoolType(v any) bool  { return isBool(v) }
func isIntType(v any) bool   { return isIntValue(v) }
func isNullType(v any) bool  { return v == nil }
func isNumType(v any) bool   { return isNumber(v) }
func isObjType(v any) bool   { return isDict(v) }
func isStrType(v any) bool   { return isStr(v) }

// Draft 6 and later integer check, an int or a float with no fractional part
func isIntOrIntegralFloat(v any) bool {
	if isIntValue(v) {
		return true
	}
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f)
}

var (
	types3 = map[string]func(any) bool{"any": isAnyType, "array": isArrayType, "boolean": isBoolType, "integer": isIntType,
		"object": isObjType, "null": isNullType, "number": isNumType, "string": isStrType}
	types4 = map[string]func(any) bool{"array": isArrayType, "boolean": isBoolType, "integer": isIntType,
		"object": isObjType, "null": isNullType, "number": isNumType, "string": isStrType}
	types6 = map[string]func(any) bool{"array": isArrayType, "boolean": isBoolType, "integer": isIntOrIntegralFloat,
		"object": isObjType, "null": isNullType, "number": isNumType, "string": isStrType}
)

// TypeChecker.is_type, UndefinedTypeCheck for an unknown name and TypeError for an unhashable one
func (d *draft) checkType(instance, t any) bool {
	switch name := t.(type) {
	case string:
		if fn, ok := d.types[name]; ok {
			return fn(instance)
		}
	case []any, *pyjson.Object:
		raise("TypeError", "unhashable type: '"+typeName(t)+"'")
	}
	raise("UndefinedTypeCheck", "Type "+repr(t)+" is unknown to this type checker")
	return false
}

var (
	draftsOnce sync.Once
	draftTable map[string]*draft
)

// Validator classes by the normalized id of their metaschema, the _META_SCHEMAS table
func drafts() map[string]*draft {
	draftsOnce.Do(func() { draftTable = buildDrafts() })
	return draftTable
}

func buildDrafts() map[string]*draft {
	d3 := &draft{name: "draft3", metaURI: "http://json-schema.org/draft-03/schema", ignoreRefSiblings: true, spec: spec3, types: types3,
		keywords: map[string]keywordFn{"$ref": kwRef, "additionalItems": kwAdditionalItems, "additionalProperties": kwAdditionalProperties,
			"dependencies": kwDependenciesDraft3, "disallow": kwDisallowDraft3, "divisibleBy": kwMultipleOf, "enum": kwEnum,
			"extends": kwExtendsDraft3, "format": kwFormat, "items": kwItemsDraft3Draft4, "maxItems": kwMaxItems, "maxLength": kwMaxLength,
			"maximum": kwMaximumDraft3Draft4, "minItems": kwMinItems, "minLength": kwMinLength, "minimum": kwMinimumDraft3Draft4,
			"pattern": kwPattern, "patternProperties": kwPatternProperties, "properties": kwPropertiesDraft3, "type": kwTypeDraft3,
			"uniqueItems": kwUniqueItems}}
	d4 := &draft{name: "draft4", metaURI: "http://json-schema.org/draft-04/schema", ignoreRefSiblings: true, spec: spec4, types: types4,
		keywords: map[string]keywordFn{"$ref": kwRef, "additionalItems": kwAdditionalItems, "additionalProperties": kwAdditionalProperties,
			"allOf": kwAllOf, "anyOf": kwAnyOf, "dependencies": kwDependenciesDraft4, "enum": kwEnum, "format": kwFormat,
			"items": kwItemsDraft3Draft4, "maxItems": kwMaxItems, "maxLength": kwMaxLength, "maxProperties": kwMaxProperties,
			"maximum": kwMaximumDraft3Draft4, "minItems": kwMinItems, "minLength": kwMinLength, "minProperties": kwMinProperties,
			"minimum": kwMinimumDraft3Draft4, "multipleOf": kwMultipleOf, "not": kwNot, "oneOf": kwOneOf, "pattern": kwPattern,
			"patternProperties": kwPatternProperties, "properties": kwProperties, "required": kwRequired, "type": kwType,
			"uniqueItems": kwUniqueItems}}
	d6 := &draft{name: "draft6", metaURI: "http://json-schema.org/draft-06/schema", ignoreRefSiblings: true, spec: spec6, types: types6,
		keywords: map[string]keywordFn{"$ref": kwRef, "additionalItems": kwAdditionalItems, "additionalProperties": kwAdditionalProperties,
			"allOf": kwAllOf, "anyOf": kwAnyOf, "const": kwConst, "contains": kwContainsDraft6Draft7, "dependencies": kwDependenciesDraft4,
			"enum": kwEnum, "exclusiveMaximum": kwExclusiveMaximum, "exclusiveMinimum": kwExclusiveMinimum, "format": kwFormat,
			"items": kwItemsDraft6To2019, "maxItems": kwMaxItems, "maxLength": kwMaxLength, "maxProperties": kwMaxProperties,
			"maximum": kwMaximum, "minItems": kwMinItems, "minLength": kwMinLength, "minProperties": kwMinProperties, "minimum": kwMinimum,
			"multipleOf": kwMultipleOf, "not": kwNot, "oneOf": kwOneOf, "pattern": kwPattern, "patternProperties": kwPatternProperties,
			"properties": kwProperties, "propertyNames": kwPropertyNames, "required": kwRequired, "type": kwType, "uniqueItems": kwUniqueItems}}
	d7 := &draft{name: "draft7", metaURI: "http://json-schema.org/draft-07/schema", ignoreRefSiblings: true, spec: spec7, types: types6,
		keywords: map[string]keywordFn{"$ref": kwRef, "additionalItems": kwAdditionalItems, "additionalProperties": kwAdditionalProperties,
			"allOf": kwAllOf, "anyOf": kwAnyOf, "const": kwConst, "contains": kwContainsDraft6Draft7, "dependencies": kwDependenciesDraft4,
			"enum": kwEnum, "exclusiveMaximum": kwExclusiveMaximum, "exclusiveMinimum": kwExclusiveMinimum, "format": kwFormat, "if": kwIf,
			"items": kwItemsDraft6To2019, "maxItems": kwMaxItems, "maxLength": kwMaxLength, "maxProperties": kwMaxProperties,
			"maximum": kwMaximum, "minItems": kwMinItems, "minLength": kwMinLength, "minProperties": kwMinProperties, "minimum": kwMinimum,
			"multipleOf": kwMultipleOf, "not": kwNot, "oneOf": kwOneOf, "pattern": kwPattern, "patternProperties": kwPatternProperties,
			"properties": kwProperties, "propertyNames": kwPropertyNames, "required": kwRequired, "type": kwType, "uniqueItems": kwUniqueItems}}
	d2019 := &draft{name: "draft2019-09", metaURI: "https://json-schema.org/draft/2019-09/schema", spec: spec201909, types: types6,
		keywords: map[string]keywordFn{"$recursiveRef": kwRecursiveRef, "$ref": kwRef, "additionalItems": kwAdditionalItems,
			"additionalProperties": kwAdditionalProperties, "allOf": kwAllOf, "anyOf": kwAnyOf, "const": kwConst, "contains": kwContains,
			"dependentRequired": kwDependentRequired, "dependentSchemas": kwDependentSchemas, "enum": kwEnum,
			"exclusiveMaximum": kwExclusiveMaximum, "exclusiveMinimum": kwExclusiveMinimum, "format": kwFormat, "if": kwIf,
			"items": kwItemsDraft6To2019, "maxItems": kwMaxItems, "maxLength": kwMaxLength, "maxProperties": kwMaxProperties,
			"maximum": kwMaximum, "minItems": kwMinItems, "minLength": kwMinLength, "minProperties": kwMinProperties, "minimum": kwMinimum,
			"multipleOf": kwMultipleOf, "not": kwNot, "oneOf": kwOneOf, "pattern": kwPattern, "patternProperties": kwPatternProperties,
			"properties": kwProperties, "propertyNames": kwPropertyNames, "required": kwRequired, "type": kwType,
			"unevaluatedItems": kwUnevaluatedItemsDraft2019, "unevaluatedProperties": kwUnevaluatedPropertiesDraft2019,
			"uniqueItems": kwUniqueItems}}
	d2020 := &draft{name: "draft2020-12", metaURI: "https://json-schema.org/draft/2020-12/schema", spec: spec202012, types: types6,
		keywords: map[string]keywordFn{"$dynamicRef": kwDynamicRef, "$ref": kwRef, "additionalProperties": kwAdditionalProperties,
			"allOf": kwAllOf, "anyOf": kwAnyOf, "const": kwConst, "contains": kwContains, "dependentRequired": kwDependentRequired,
			"dependentSchemas": kwDependentSchemas, "enum": kwEnum, "exclusiveMaximum": kwExclusiveMaximum,
			"exclusiveMinimum": kwExclusiveMinimum, "format": kwFormat, "if": kwIf, "items": kwItems, "maxItems": kwMaxItems,
			"maxLength": kwMaxLength, "maxProperties": kwMaxProperties, "maximum": kwMaximum, "minItems": kwMinItems,
			"minLength": kwMinLength, "minProperties": kwMinProperties, "minimum": kwMinimum, "multipleOf": kwMultipleOf, "not": kwNot,
			"oneOf": kwOneOf, "pattern": kwPattern, "patternProperties": kwPatternProperties, "prefixItems": kwPrefixItems,
			"properties": kwProperties, "propertyNames": kwPropertyNames, "required": kwRequired, "type": kwType,
			"unevaluatedItems": kwUnevaluatedItems, "unevaluatedProperties": kwUnevaluatedProperties, "uniqueItems": kwUniqueItems}}
	out := map[string]*draft{}
	docs, _ := specifications()
	for _, d := range []*draft{d3, d4, d6, d7, d2019, d2020} {
		out[normalizeURI(d.spec.idOf(docs[d.metaURI]))] = d
	}
	return out
}

func latestDraft() *draft {
	return drafts()[normalizeURI("https://json-schema.org/draft/2020-12/schema")]
}

// validator_for, the class a schema's $schema names or the default
func validatorFor(schema any, def *draft, st *state) *draft {
	st.touch(validatorForDepth(schema))
	switch s := schema.(type) {
	case bool:
		return def
	case *pyjson.Object:
		dialect, ok := s.Vals["$schema"]
		if !ok {
			return def
		}
		if d, known := drafts()[normalizeURI(dialect)]; known {
			return d
		}
		return def
	case string:
		if !strings.Contains(s, "$schema") {
			return def
		}
		raise("TypeError", "string indices must be integers, not 'str'")
	case []any:
		if !pyIn("$schema", s) {
			return def
		}
		raise("TypeError", "list indices must be integers or slices, not str")
	}
	raise("TypeError", "argument of type '"+typeName(schema)+"' is not a container or iterable")
	return nil
}

// Keyword and value pairs a schema applies, $ref alone for drafts that ignore its siblings
func (d *draft) applicable(schema any, st *state) [][2]any {
	if d.ignoreRefSiblings {
		defer st.call()()
		if ref := pyGet(schema, "$ref"); ref != nil {
			return [][2]any{{"$ref", ref}}
		}
	}
	o, ok := schema.(*pyjson.Object)
	if !ok {
		raise("AttributeError", "'"+typeName(schema)+"' object has no attribute 'items'")
	}
	out := make([][2]any, len(o.Keys))
	for i, k := range o.Keys {
		out[i] = [2]any{k, o.Vals[k]}
	}
	return out
}

// re.compile of a pattern, re.error and OverflowError raised as Python raises them
func (st *state) compile(pattern string) *regexp2.Regexp {
	if re, ok := st.regexes[pattern]; ok {
		return re
	}
	re, err := pyre.Compile(pattern)
	if err != nil {
		var overflow *pyre.OverflowError
		if errors.As(err, &overflow) {
			raise("OverflowError", overflow.Error())
		}
		raise("PatternError", err.Error())
	}
	st.regexes[pattern] = re
	return re
}

// re.search of a pattern over a str, search and _compile on the stack
func (st *state) search(pattern, s string) bool {
	st.touch(2)
	ok, err := st.compile(pattern).MatchRunes(pystr.Runes(s))
	if err != nil {
		raise("TimeoutError", err.Error())
	}
	return ok
}

// Schema applied with the resolver and format checker an evolved Python validator carries
type validator struct {
	cls      *draft
	schema   any
	resolver *resolver
	formats  bool
	bound    [][2]any
	st       *state
}

// Validator construction, __init__ and __attrs_post_init__, a root resolver made when none is given
func newValidator(cls *draft, schema any, res *resolver, formats bool, st *state) *validator {
	st.enter(2)
	defer st.leave(2)
	if res == nil {
		st.touch(2)
		res = rootResolver(cls.spec.createResource(schema), st)
	}
	v := &validator{cls: cls, schema: schema, resolver: res, formats: formats, st: st}
	if _, isBool := schema.(bool); !isBool {
		for _, kv := range cls.applicable(schema, st) {
			if _, ok := cls.keywords[kv[0].(string)]; ok {
				v.bound = append(v.bound, kv)
			}
		}
	}
	return v
}

// Registry of the shipped metaschemas able to retrieve remote resources, the one jsonschema validators use
var remoteRegistry = sync.OnceValue(func() *registry {
	_, specs := specifications()
	g := specs.withResources()
	g.uncrawled = nil
	g.retrieve = retrieveRemote
	return g
})

// resolver_with_root, the root registered under its id and a resolver based there
func rootResolver(r *resource, st *state) *resolver {
	defer st.call()()
	uri, _ := r.id(st)
	st.touch(2 + attrsEvolveDepth)
	st.touch(1)
	return &resolver{baseURI: uri, registry: remoteRegistry().withResources(uriResource{uri, r}), st: st}
}

func (v *validator) evolve(schema any, res *resolver) *validator {
	defer v.st.call()()
	cls := validatorFor(schema, v.cls, v.st)
	if res == nil {
		res = v.resolver
	}
	return newValidator(cls, schema, res, v.formats, v.st)
}

// Validator.is_type, an unknown type raised as UnknownType
func (v *validator) isType(instance, t any) bool {
	switch t.(type) {
	case []any, *pyjson.Object:
		v.st.touch(2)
	default:
		v.st.touch(v.cls.isTypeDepth(instance, t))
	}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Exception); ok && e.Type == "UndefinedTypeCheck" {
				schema := v.schema
				panic(&Exception{Type: "UnknownType", render: func(st *state) string {
					defer st.call()()
					return "Unknown type " + repr(t) + " for validator with schema:\n    " + prettyAt(st, schema) +
						"\n\nWhile checking instance:\n    " + prettyAt(st, instance)
				}})
			}
			panic(r)
		}
	}()
	return v.cls.checkType(instance, t)
}

// ValidationError creation inside a keyword, _Error.__init__ on the stack
func (v *validator) errorf(message string) *vError {
	v.st.touch(1)
	return newError(message)
}

// iter_errors, every error of the instance in schema keyword order
func (v *validator) iterErrors(instance any) iter.Seq[*vError] {
	st := v.st
	return st.gen(func(yield func(*vError) bool) {
		switch v.schema {
		case true:
			return
		case false:
			st.touch(1)
			yield(newFullError("False schema does not allow "+repr(instance), nil, nil, instance, v.schema))
			return
		}
		schema := v.schema.(*pyjson.Object)
		for _, kv := range v.bound {
			k := kv[0].(string)
			for e := range st.gen(v.cls.keywords[k](v, kv[1], instance, schema)) {
				st.touch(1)
				e.setDetails(k, kv[1], instance, schema, v.cls)
				if k != "if" && k != "$ref" {
					e.schemaPath = prepend(e.schemaPath, k)
				}
				if !yield(e) {
					return
				}
			}
		}
	})
}

// Subschema errors with instance and schema path prefixes, nil meaning no prefix
func (v *validator) descend(instance, schema, path, schemaPath any, res *resolver) iter.Seq[*vError] {
	st := v.st
	return st.gen(func(yield func(*vError) bool) {
		switch schema {
		case true:
			return
		case false:
			st.touch(1)
			yield(newFullError("False schema does not allow "+repr(instance), nil, nil, instance, schema))
			return
		}
		if res == nil {
			st.touch(2)
			res = v.resolver.inSubresource(v.cls.spec.createResource(schema))
		}
		evolved := v.evolve(schema, res)
		for _, kv := range v.cls.applicable(schema, st) {
			k := kv[0].(string)
			fn := evolved.cls.keywords[k]
			if fn == nil {
				continue
			}
			for e := range st.gen(fn(evolved, kv[1], instance, schema.(*pyjson.Object))) {
				st.touch(1)
				e.setDetails(k, kv[1], instance, schema, evolved.cls)
				if k != "if" && k != "$ref" {
					e.schemaPath = prepend(e.schemaPath, k)
				}
				if path != nil {
					e.path = prepend(e.path, path)
				}
				if schemaPath != nil {
					e.schemaPath = prepend(e.schemaPath, schemaPath)
				}
				if !yield(e) {
					return
				}
			}
		}
	})
}

// Validator.is_valid, a frame of its own around the first step of iter_errors
func (v *validator) isValid(instance any) bool {
	defer v.st.call()()
	for range v.iterErrors(instance) {
		return false
	}
	return true
}

// next over an error iterator, whether it yields nothing
func isValidSeq(errs iter.Seq[*vError]) bool {
	for range errs {
		return false
	}
	return true
}

// _utils.is_valid, the same check from inside a function frame
func (v *validator) isValidFn(errs iter.Seq[*vError]) bool {
	defer v.st.call()()
	return isValidSeq(errs)
}

// _validate_reference, an unresolvable reference raised wrapped the way jsonschema wraps it
func (v *validator) validateReference(ref any, instance any) iter.Seq[*vError] {
	return func(yield func(*vError) bool) {
		contents, res := v.lookupWrapped(ref)
		yieldFrom(v.descend(instance, contents, nil, nil, res), yield)
	}
}

func (v *validator) lookupWrapped(ref any) (contents any, res *resolver) {
	defer v.st.call()()
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Exception); ok {
				switch e.Type {
				case "Unresolvable", "PointerToNowhere", "NoSuchAnchor", "InvalidAnchor":
					raise("_WrappedReferencingError", e.Type+": "+e.Msg)
				}
			}
			panic(r)
		}
	}()
	return v.lookup(ref)
}

func (v *validator) lookup(ref any) (any, *resolver) {
	s, ok := ref.(string)
	if !ok {
		raise("AttributeError", "'"+typeName(ref)+"' object has no attribute 'startswith'")
	}
	return v.resolver.lookup(s)
}

// check_schema, the first metaschema error raised as a SchemaError
func (d *draft) checkSchema(schema any, st *state) {
	defer st.call()()
	docs, _ := specifications()
	meta := docs[d.metaURI]
	cls := validatorFor(meta, d, st)
	for e := range newValidator(cls, meta, nil, true, st).iterErrors(schema) {
		st.touch(2)
		panic(&SchemaError{Message: e.message, err: e})
	}
}
