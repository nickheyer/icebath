package dashboard

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var preferencesPatchModel = &Model{ClassName: "PreferencesPatch", Fields: []ModelField{
	FieldFactory("values", DictOf(Optional(Str)), func() any { return pyjson.NewObject() }),
	FieldDefault("seed", Bool, false),
}}

// Route error of a store failure, 400 for a refused write and 503 for a busy store
func uiPreferencesHTTPError(err error, refusedIs400 bool) error {
	var pe *preferencesError
	if refusedIs400 && errors.As(err, &pe) {
		return NewHTTPException(400, pe.Error())
	}
	var busy *preferencesBusy
	if errors.As(err, &busy) {
		return NewHTTPException(503, busy.Error())
	}
	return err
}

// Router of the machine UI preferences store
func uiPreferencesRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/ui-preferences", &Endpoint{
		Name: "get_ui_preferences",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			snap, err := readPreferences(ctx, "")
			if err != nil {
				return nil, uiPreferencesHTTPError(err, false)
			}
			return snap, nil
		},
	})
	r.Patch("/api/ui-preferences", &Endpoint{
		Name: "patch_ui_preferences",
		Body: []BodyParam{Body("body", preferencesPatchModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			values, _ := body.GetOr("values", nil).(*pyjson.Object)
			snap, err := writePreferences(ctx, values, body.GetOr("seed", false) == true, "")
			if err != nil {
				return nil, uiPreferencesHTTPError(err, true)
			}
			return snap, nil
		},
	})
	return r
}
