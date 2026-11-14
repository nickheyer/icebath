package providers

func newKilocode() Profile {
	return &base{spec: Spec{
		Name: "kilocode", Aliases: []string{"kilo-code", "kilo", "kilo-gateway"}, EnvVars: []string{"KILOCODE_API_KEY"},
		BaseURL: "https://api.kilo.ai/api/gateway", DefaultAuxModel: "google/gemini-3.6-flash",
	}}
}
