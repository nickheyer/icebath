package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/observability"
)

var consentAnswerModel = &Model{ClassName: "ConsentAnswer", Fields: []ModelField{
	Field("enabled", Bool),
	FieldDefault("send", Bool, false),
}}

// Enabled, send, decided and reask of the scoped profile plus whether the install is managed
func readConsent(ctx context.Context) (*pyjson.Object, error) {
	h := home.Dir(ctx)
	raw, err := config.ReadRawConfig(ctx, h)
	if err != nil {
		return nil, err
	}
	s := observability.ConsentState(raw)
	return pyjson.ObjectOf("enabled", s.Enabled, "send", s.Send, "decided", s.Decided, "reask", s.Reask,
		"managed", config.ManagedSystem(ctx, h) != ""), nil
}

// Router of the first run shared metrics consent read and write
func sharedMetricsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/shared-metrics/consent", &Endpoint{
		Name:   "get_shared_metrics_consent",
		Params: []Param{QueryParam("profile", Optional(Str), nil)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getSharedMetricsConsent(ctx, c.Str("profile")) },
	})
	r.Put("/api/shared-metrics/consent", &Endpoint{
		Name:   "put_shared_metrics_consent",
		Params: []Param{QueryParam("profile", Optional(Str), nil)},
		Body:   []BodyParam{Body("body", consentAnswerModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return putSharedMetricsConsent(ctx, c.Object("body"), c.Str("profile"))
		},
	})
	return r
}

// Consent answer of the requested profile
func getSharedMetricsConsent(ctx context.Context, profile string) (*pyjson.Object, error) {
	return scopedToThread(ctx, profile, readConsent)
}

// Saves the consent answer to the requested profile, returning the state read back
func putSharedMetricsConsent(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	sctx, release, err := configWriteScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	enabled, _ := body.Vals["enabled"].(bool)
	send, _ := body.Vals["send"].(bool)
	if err := observability.SaveConsent(sctx, enabled, send, nil); err != nil {
		return nil, err
	}
	return readConsent(sctx)
}
