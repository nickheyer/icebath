package providers

func newXiaomi() Profile {
	return &base{spec: Spec{
		Name: "xiaomi", Aliases: []string{"mimo", "xiaomi-mimo"}, EnvVars: []string{"XIAOMI_API_KEY"},
		BaseURL: "https://api.xiaomimimo.com/v1", SkipHealthCheck: true, Vision: true, RejectsVisionToolMessages: true,
	}}
}
