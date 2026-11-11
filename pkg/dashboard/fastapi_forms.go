package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Upload parameter type accepting only a parsed upload, the FastAPI UploadFile
type uploadFileType struct{}

func (uploadFileType) isSequence() bool { return false }

// The upload itself, a value error naming the received type otherwise
func (uploadFileType) Validate(v any) (any, []ErrorDetails) {
	if u, ok := v.(*UploadFile); ok {
		return u, nil
	}
	return nil, []ErrorDetails{{Type: "value_error", Loc: []any{},
		Msg:   "Value error, Expected UploadFile, received: <class '" + pyrepr.TypeName(v) + "'>",
		Input: v, Ctx: jsonObj("error", pyjson.NewObject())}}
}

// Form or File field info around the annotated type of a body parameter
type formFieldType struct {
	inner FieldType
	file  bool
}

func (f formFieldType) isSequence() bool { return f.inner.isSequence() }

// Validation of the annotated type
func (f formFieldType) Validate(v any) (any, []ErrorDetails) { return f.inner.Validate(v) }

// Form parameters embed under their name unless the annotation is a model
func formEmbed(t FieldType) bool {
	_, isModel := t.(*Model)
	return !isModel
}

// Required Form parameter of a type
func FormField(name string, t FieldType) BodyParam {
	return BodyParam{Name: name, Type: formFieldType{inner: t}, Required: true, Embed: formEmbed(t)}
}

// Form parameter of a type with a default
func FormFieldDefault(name string, t FieldType, def any) BodyParam {
	return BodyParam{Name: name, Type: formFieldType{inner: t}, Default: def, Embed: formEmbed(t)}
}

// Required File parameter holding one upload
func FileField(name string) BodyParam {
	return BodyParam{Name: name, Type: formFieldType{inner: uploadFileType{}, file: true}, Required: true, Embed: true}
}

// Whether the body is read as a form and whether any parameter is a File
func formBodyKind(body []BodyParam) (bool, bool) {
	isForm, isFile := false, false
	for _, p := range body {
		if f, ok := p.Type.(formFieldType); ok {
			isForm = true
			isFile = isFile || f.file
		}
	}
	return isForm, isFile
}

// Form field as extraction sees it, its alias, annotated type and Form field info
type formExtractField struct {
	alias    string
	typ      FieldType
	isForm   bool
	required bool
	def      any
}

// Multidict value of a field, a Form empty string or empty sequence reading as missing or the default
func getMultidictValue(f formExtractField, form *FormData) any {
	var value any
	isSeq := f.typ.isSequence()
	if isSeq {
		if list := form.GetList(f.alias); list != nil {
			value = list
		} else {
			value = []any{}
		}
	} else if v, ok := form.Get(f.alias); ok {
		value = v
	}
	empty := value == nil
	if s, ok := value.(string); ok && f.isForm && s == "" {
		empty = true
	}
	if list, ok := value.([]any); ok && isSeq && len(list) == 0 {
		empty = true
	}
	if empty {
		if f.required {
			return nil
		}
		return pyjson.Clone(f.def)
	}
	return value
}

// Body values of a form for the fields, keys no field names copied as one value or a list
func extractFormBody(fields []formExtractField, form *FormData) *pyjson.Object {
	values := pyjson.NewObject()
	aliases := map[string]bool{}
	for _, f := range fields {
		if value := getMultidictValue(f, form); value != nil {
			values.Set(f.alias, value)
		}
		aliases[f.alias] = true
	}
	for _, key := range form.Keys() {
		if aliases[key] {
			continue
		}
		list := form.GetList(key)
		if len(list) == 1 {
			values.Set(key, list[0])
		} else {
			values.Set(key, list)
		}
	}
	return values
}

// Fields a form body is extracted for, the model fields of a lone unembedded model parameter
func formExtractFields(body []BodyParam) []formExtractField {
	if len(body) == 1 && !embedBodyFields(body) {
		if f, ok := body[0].Type.(formFieldType); ok {
			if m, isModel := f.inner.(*Model); isModel {
				out := make([]formExtractField, 0, len(m.Fields))
				for _, mf := range m.Fields {
					out = append(out, formExtractField{alias: mf.validationAlias(), typ: mf.Type, required: mf.Required, def: mf.defaultValue()})
				}
				return out
			}
		}
	}
	out := make([]formExtractField, 0, len(body))
	for _, p := range body {
		_, isForm := p.Type.(formFieldType)
		out = append(out, formExtractField{alias: p.Name, typ: p.Type, isForm: isForm, required: p.Required, def: p.Default})
	}
	return out
}

// Body of a request for an operation with the closer of its form, the extracted form or the JSON body
func readRequestBody(ctx context.Context, req *Request, ep *Endpoint) (any, func(), error) {
	if isForm, _ := formBodyKind(ep.Body); !isForm {
		v, err := readJSONBody(ctx, req)
		return v, func() {}, err
	}
	form, err := req.Form(ctx, 1000, 1000, 1024*1024)
	if err != nil {
		if h, ok := err.(*HTTPException); ok {
			return nil, func() {}, h
		}
		return nil, func() {}, NewHTTPException(400, "There was an error parsing the body")
	}
	return extractFormBody(formExtractFields(ep.Body), form), func() { form.Close() }, nil
}

// JSON schema of a form parameter, an upload as an octet stream string
func formFieldSchema(t FieldType, defs map[string]*pyjson.Object) *pyjson.Object {
	if f, ok := t.(formFieldType); ok {
		t = f.inner
	}
	if _, ok := t.(uploadFileType); ok {
		return schemaObj("type", "string", "contentMediaType", "application/octet-stream")
	}
	return typeSchema(t, defs)
}

// OpenAPI request body of a form operation, urlencoded or multipart when a File is present, nil for any other body
func formRequestBody(r *APIRoute, ep *Endpoint, defs map[string]*pyjson.Object) *pyjson.Object {
	isForm, isFile := formBodyKind(ep.Body)
	if !isForm {
		return nil
	}
	mediaType := "application/x-www-form-urlencoded"
	if isFile {
		mediaType = "multipart/form-data"
	}
	var bodySchema *pyjson.Object
	required := false
	if !embedBodyFields(ep.Body) {
		p := ep.Body[0]
		bodySchema = formFieldSchema(p.Type, defs)
		if f, ok := p.Type.(formFieldType); ok {
			if _, isModel := f.inner.(*Model); !isModel {
				bodySchema = schemaWith(bodySchema, "title", fieldTitle(p.Name))
			}
		}
		required = p.Required
	} else {
		name := "Body_" + uniqueID(r)
		props := pyjson.NewObject()
		var req []any
		for _, p := range ep.Body {
			s := formFieldSchema(p.Type, defs)
			if f, ok := p.Type.(formFieldType); !ok || !isModelType(f.inner) {
				s = schemaWith(s, "title", fieldTitle(p.Name))
			}
			if p.Required {
				req = append(req, p.Name)
			} else if p.Default != nil {
				s = schemaWith(s, "default", jsonableEncoder(p.Default))
			}
			props.Set(p.Name, s)
			required = required || p.Required
		}
		var reqV any
		if len(req) > 0 {
			reqV = req
		}
		defs[name] = schemaObj("properties", props, "type", "object", "required", reqV, "title", name)
		bodySchema = schemaObj("$ref", refPrefix+name)
	}
	rb := pyjson.NewObject()
	rb.Set("content", jsonObj(mediaType, jsonObj("schema", bodySchema)))
	if required {
		rb.Set("required", true)
	}
	return rb
}

// Whether a field type is a pydantic model
func isModelType(t FieldType) bool {
	_, ok := t.(*Model)
	return ok
}
