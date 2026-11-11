package dashboard

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	runtimeInstallBodyModel = &Model{ClassName: "RuntimeInstallBody", Fields: []ModelField{FieldDefault("backend", optStr, nil)}}
	modelDownloadBodyModel  = &Model{ClassName: "ModelDownloadBody", Fields: []ModelField{Field("model_id", Str)}}
	jobIDBodyModel          = &Model{ClassName: "JobIdBody", Fields: []ModelField{Field("job_id", Str)}}
	quickstartBodyModel     = &Model{ClassName: "QuickstartBody", Fields: []ModelField{FieldDefault("model_id", optStr, nil)}}
	serverActionBodyModel   = &Model{ClassName: "ServerActionBody", Fields: []ModelField{Field("action", Str)}}
	modelEjectBodyModel     = &Model{ClassName: "ModelEjectBody", Fields: []ModelField{Field("model_id", Str)}}
	modelActivateBodyModel  = &Model{ClassName: "ModelActivateBody", Fields: []ModelField{Field("model_id", Str)}}
	browsedDownloadModel    = &Model{ClassName: "BrowsedDownloadBody", Fields: []ModelField{Field("repo", Str), Field("paths", ListOf(Str))}}
	sideloadBodyModel       = &Model{ClassName: "SideloadBody", Fields: []ModelField{Field("path", Str)}}
)

// JSON answer of a local models handler, its HTTP refusal turned into the dashboard one
func localModelsAnswer(result map[string]any, err error) (any, error) {
	if err != nil {
		return nil, asHTTPError(err)
	}
	return configJSON(result), nil
}

// Local models routes over the managed llama.cpp runtime handlers in their declaration order
func localModelsRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/local-models/status", &Endpoint{
		Name: "local_models_status",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsStatus(ctx))
		},
	})
	r.Get("/api/local-models/hardware", &Endpoint{
		Name: "local_models_hardware",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsHardware(ctx))
		},
	})
	r.Get("/api/local-models/catalog", &Endpoint{
		Name: "local_models_catalog",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsCatalog(ctx))
		},
	})
	r.Post("/api/local-models/runtime/install", &Endpoint{
		Name:   "local_models_runtime_install",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", runtimeInstallBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsRuntimeInstall(ctx, bodyStr(c.Object("body"), "backend"), profileQuery(c)))
		},
	})
	r.Post("/api/local-models/download", &Endpoint{
		Name: "local_models_download",
		Body: []BodyParam{Body("body", modelDownloadBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsDownload(ctx, bodyStr(c.Object("body"), "model_id")))
		},
	})
	r.Post("/api/local-models/download/pause", &Endpoint{
		Name: "local_models_download_pause",
		Body: []BodyParam{Body("body", jobIDBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsDownloadPause(ctx, bodyStr(c.Object("body"), "job_id")))
		},
	})
	r.Post("/api/local-models/download/resume", &Endpoint{
		Name: "local_models_download_resume",
		Body: []BodyParam{Body("body", jobIDBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsDownloadResume(ctx, bodyStr(c.Object("body"), "job_id")))
		},
	})
	r.Delete("/api/local-models/models/{model_id}", &Endpoint{
		Name:   "local_models_delete",
		Params: []Param{PathParam("model_id", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsDelete(ctx, c.Str("model_id")))
		},
	})
	r.Post("/api/local-models/quickstart", &Endpoint{
		Name:   "local_models_quickstart",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", quickstartBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsQuickstart(ctx, bodyStr(c.Object("body"), "model_id"), profileQuery(c)))
		},
	})
	r.Post("/api/local-models/server", &Endpoint{
		Name: "local_models_server",
		Body: []BodyParam{Body("body", serverActionBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsServer(ctx, bodyStr(c.Object("body"), "action")))
		},
	})
	r.Post("/api/local-models/eject", &Endpoint{
		Name: "local_models_eject",
		Body: []BodyParam{Body("body", modelEjectBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsEject(ctx, bodyStr(c.Object("body"), "model_id")))
		},
	})
	r.Post("/api/local-models/activate", &Endpoint{
		Name:   "local_models_activate",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", modelActivateBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsActivate(ctx, bodyStr(c.Object("body"), "model_id"), profileQuery(c)))
		},
	})
	r.Get("/api/local-models/jobs", &Endpoint{
		Name: "local_models_jobs",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsJobs(ctx))
		},
	})
	r.Get("/api/local-models/jobs/{job_id}", &Endpoint{
		Name:   "local_models_job",
		Params: []Param{PathParam("job_id", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsJob(ctx, c.Str("job_id")))
		},
	})
	r.Get("/api/local-models/search", &Endpoint{
		Name:   "local_models_search",
		Params: []Param{RequiredQuery("q", Str), QueryParam("limit", Int, int64(20))},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsSearch(ctx, c.Str("q"), int(c.Int("limit"))))
		},
	})
	r.Get("/api/local-models/search/files", &Endpoint{
		Name:   "local_models_search_files",
		Params: []Param{RequiredQuery("repo", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsSearchFiles(ctx, c.Str("repo")))
		},
	})
	r.Post("/api/local-models/download-browsed", &Endpoint{
		Name: "local_models_download_browsed",
		Body: []BodyParam{Body("body", browsedDownloadModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			body := c.Object("body")
			paths := []string{}
			items, _ := body.GetOr("paths", nil).([]any)
			for _, item := range items {
				paths = append(paths, item.(string))
			}
			return localModelsAnswer(provider.LocalModelsDownloadBrowsed(ctx, bodyStr(body, "repo"), paths))
		},
	})
	r.Post("/api/local-models/sideload", &Endpoint{
		Name: "local_models_sideload",
		Body: []BodyParam{Body("body", sideloadBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return localModelsAnswer(provider.LocalModelsSideload(ctx, bodyStr(c.Object("body"), "path")))
		},
	})
	return r
}
