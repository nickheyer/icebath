package jsonschemautil

import (
	"context"
	"strings"
	"sync"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// JSON Schema dialect, named by the year or number of its draft
type Draft int

const (
	// Dialect of draft 3
	Draft3 Draft = 3
	// Dialect of draft 4
	Draft4 Draft = 4
	// Dialect of draft 6
	Draft6 Draft = 6
	// Dialect of draft 7
	Draft7 Draft = 7
	// Dialect of draft 2019-09
	Draft201909 Draft = 2019
	// Dialect of draft 2020-12, the default
	Draft202012 Draft = 2020
)

// Where a keyword holds subschemas
type position uint8

const (
	// The value is a schema
	posSchema position = iota + 1
	// The value is an array of schemas
	posArray
	// The value is an object whose member values are schemas
	posMap
	// The value is a schema or an array of schemas
	posEither
)

// Rules of one dialect: its keywords, where they hold subschemas, and how resources and anchors are named
type dialect struct {
	draft Draft
	// Metaschema URI without its fragment
	uri   string
	idKey string
	// A fragment-only id names an anchor, and an id beside $ref is ignored
	legacyID bool
	// $ref makes the other keywords of its schema inert
	refAlone  bool
	anchors   bool
	dynamic   bool
	recursive bool
	// contains marks the items it matches as evaluated for unevaluatedItems
	containsEvaluates bool
	anyType           bool
	keywords          map[string]bool
	subschemas        map[string]position

	metaOnce sync.Once
	meta     *Schema
	metaErr  error
}

var (
	dialectsOnce sync.Once
	dialectTable map[Draft]*dialect
)

// Dialect of a draft, nil for a value that names none
func dialectOf(d Draft) *dialect {
	dialectsOnce.Do(buildDialects)
	return dialectTable[d]
}

func buildDialects() {
	words := func(s string) map[string]bool {
		m := map[string]bool{}
		for w := range strings.FieldsSeq(s) {
			m[w] = true
		}
		return m
	}
	places := func(schema, array, object, either string) map[string]position {
		m := map[string]position{}
		for _, group := range []struct {
			names string
			pos   position
		}{{schema, posSchema}, {array, posArray}, {object, posMap}, {either, posEither}} {
			for w := range strings.FieldsSeq(group.names) {
				m[w] = group.pos
			}
		}
		return m
	}
	const (
		keywords4 = "$ref additionalItems additionalProperties allOf anyOf dependencies enum format items maxItems maxLength " +
			"maxProperties maximum minItems minLength minProperties minimum multipleOf not oneOf pattern patternProperties " +
			"properties required type uniqueItems"
		keywords6       = keywords4 + " const contains exclusiveMaximum exclusiveMinimum propertyNames"
		keywordsCurrent = "$ref additionalProperties allOf anyOf const contains contentEncoding contentMediaType contentSchema " +
			"dependentRequired dependentSchemas enum exclusiveMaximum exclusiveMinimum format if items maxItems maxLength " +
			"maxProperties maximum minItems minLength minProperties minimum multipleOf not oneOf pattern patternProperties " +
			"properties propertyNames required type unevaluatedItems unevaluatedProperties uniqueItems"
		objectsLegacy  = "definitions patternProperties properties dependencies"
		objectsCurrent = "$defs definitions dependentSchemas patternProperties properties"
	)
	dialectTable = map[Draft]*dialect{
		Draft3: {draft: Draft3, uri: "http://json-schema.org/draft-03/schema", idKey: "id", legacyID: true, refAlone: true, anyType: true,
			keywords: words("$ref additionalItems additionalProperties dependencies disallow divisibleBy enum extends format items " +
				"maxItems maxLength maximum minItems minLength minimum pattern patternProperties properties type uniqueItems"),
			subschemas: places("additionalItems additionalProperties", "", objectsLegacy, "items extends type disallow")},
		Draft4: {draft: Draft4, uri: "http://json-schema.org/draft-04/schema", idKey: "id", legacyID: true, refAlone: true,
			keywords:   words(keywords4),
			subschemas: places("additionalItems additionalProperties not", "allOf anyOf oneOf", objectsLegacy, "items")},
		Draft6: {draft: Draft6, uri: "http://json-schema.org/draft-06/schema", idKey: "$id", legacyID: true, refAlone: true,
			keywords:   words(keywords6),
			subschemas: places("additionalItems additionalProperties contains not propertyNames", "allOf anyOf oneOf", objectsLegacy, "items")},
		Draft7: {draft: Draft7, uri: "http://json-schema.org/draft-07/schema", idKey: "$id", legacyID: true, refAlone: true,
			keywords: words(keywords6 + " if contentEncoding contentMediaType"),
			subschemas: places("additionalItems additionalProperties contains else if not propertyNames then", "allOf anyOf oneOf",
				objectsLegacy, "items")},
		Draft201909: {draft: Draft201909, uri: "https://json-schema.org/draft/2019-09/schema", idKey: "$id", anchors: true, recursive: true,
			keywords: words(keywordsCurrent + " $recursiveRef additionalItems"),
			subschemas: places("additionalItems additionalProperties contains contentSchema else if not propertyNames then "+
				"unevaluatedItems unevaluatedProperties", "allOf anyOf oneOf", objectsCurrent, "items")},
		Draft202012: {draft: Draft202012, uri: "https://json-schema.org/draft/2020-12/schema", idKey: "$id", anchors: true, dynamic: true,
			containsEvaluates: true,
			keywords:          words(keywordsCurrent + " $dynamicRef prefixItems"),
			subschemas: places("additionalProperties contains contentSchema else if items not propertyNames then "+
				"unevaluatedItems unevaluatedProperties", "allOf anyOf oneOf prefixItems", objectsCurrent, "")},
	}
}

// Dialect a $schema URI names, http or https and a trailing # alike; nil for a URI that names none the package knows
func dialectFor(uri string) *dialect {
	u := strings.TrimSuffix(strings.TrimSpace(uri), "#")
	if rest, ok := strings.CutPrefix(u, "https://"); ok {
		u = rest
	} else if rest, ok := strings.CutPrefix(u, "http://"); ok {
		u = rest
	} else {
		return nil
	}
	switch u {
	case "json-schema.org/draft/2020-12/schema", "json-schema.org/schema":
		return dialectOf(Draft202012)
	case "json-schema.org/draft/2019-09/schema":
		return dialectOf(Draft201909)
	case "json-schema.org/draft-07/schema":
		return dialectOf(Draft7)
	case "json-schema.org/draft-06/schema":
		return dialectOf(Draft6)
	case "json-schema.org/draft-04/schema":
		return dialectOf(Draft4)
	case "json-schema.org/draft-03/schema":
		return dialectOf(Draft3)
	}
	return nil
}

// Dialect a document names in $schema, fallback when it names none the package knows
func docDialect(doc any, fallback *dialect) *dialect {
	if o, ok := doc.(*jsonutil.Object); ok {
		if s, ok := o.Value("$schema").(string); ok {
			if d := dialectFor(s); d != nil {
				return d
			}
		}
	}
	return fallback
}

// Identifier a schema object declares for itself, false when it declares none or the dialect ignores it there
func (d *dialect) id(o *jsonutil.Object) (string, bool) {
	s, ok := o.Value(d.idKey).(string)
	if !ok || s == "" {
		return "", false
	}
	if d.legacyID && o.Has("$ref") {
		return "", false
	}
	return s, true
}

// Bundled metaschema of the dialect, compiled on first use
func (d *dialect) metaschema() (*Schema, error) {
	d.metaOnce.Do(func() {
		res := bundled().resources[d.uri]
		cfg := &config{draft: d.draft, timeout: defaultMatchTimeout, skipMeta: true,
			formats: map[string]func(string) bool{"regex": validPattern}}
		d.meta, d.metaErr = compileDoc(context.Background(), res.root, cfg)
	})
	return d.meta, d.metaErr
}
