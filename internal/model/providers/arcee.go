package providers

type base struct {
	spec Spec
}

func (b *base) Spec() *Spec { return &b.spec }

func newArcee() Profile {
	return &base{spec: Spec{
		Name: "arcee", Aliases: []string{"arcee-ai", "arceeai"}, EnvVars: []string{"ARCEEAI_API_KEY"},
		BaseURL: "https://api.arcee.ai/api/v1",
	}}
}
