// Package pyjsonschema ports python-jsonschema 4.26.0 with referencing 0.37.0 so validation errors read as Python renders them
package pyjsonschema

// One error of an iter_errors pass with its message and absolute instance path
type IterError struct {
	Message string
	// Keyword that failed, nil for a false schema
	Validator    any
	AbsolutePath []any
}

// Every error validator_for(schema)(schema).iter_errors(instance) yields, called from callerDepth Python frames deep
func IterErrors(instance, schema any, callerDepth int) ([]*IterError, error) {
	inst, sch := pyValue(instance), pyValue(schema)
	var out []*IterError
	err := catch(func() {
		st := newState(callerDepth)
		cls := validatorFor(sch, latestDraft(), st)
		for e := range newValidator(cls, sch, nil, false, st).iterErrors(inst) {
			out = append(out, &IterError{Message: e.message, Validator: e.validator, AbsolutePath: e.absolutePath()})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validator_for(schema).check_schema(schema), the first metaschema error as a SchemaError
func CheckSchema(schema any, callerDepth int) error {
	sch := pyValue(schema)
	return catch(func() {
		st := newState(callerDepth)
		validatorFor(sch, latestDraft(), st).checkSchema(sch, st)
	})
}

// validator_for, check_schema and best_match over iter_errors run from callerDepth Python frames deep, nil when valid
func BestMatch(instance, schema any, callerDepth int) (*ValidationError, error) {
	inst, sch := pyValue(instance), pyValue(schema)
	var best *vError
	err := catch(func() {
		best = bestMatchAt(inst, sch, newState(callerDepth))
	})
	if err != nil {
		return nil, err
	}
	if best == nil {
		return nil, nil
	}
	return publicError(best), nil
}

func bestMatchAt(inst, sch any, st *state) *vError {
	cls := validatorFor(sch, latestDraft(), st)
	cls.checkSchema(sch, st)
	return bestMatch(newValidator(cls, sch, nil, false, st).iterErrors(inst), st)
}

// jsonschema.validate called from callerDepth Python frames deep, raising the SchemaError or the best matching ValidationError
func Validate(instance, schema any, callerDepth int) error {
	inst, sch := pyValue(instance), pyValue(schema)
	var best *vError
	err := catch(func() {
		st := newState(callerDepth)
		defer st.call()()
		best = bestMatchAt(inst, sch, st)
	})
	if err != nil {
		return err
	}
	if best != nil {
		return publicError(best)
	}
	return nil
}
