package providers

func newHuggingFace() Profile {
	return &base{spec: Spec{
		Name: "huggingface", Aliases: []string{"hf", "hugging-face", "huggingface-hub"}, EnvVars: []string{"HF_TOKEN"},
		DisplayName: "HuggingFace", Description: "HuggingFace Inference API", SignupURL: "https://huggingface.co/settings/tokens",
		FallbackModels: []string{"Qwen/Qwen3.5-72B-Instruct", "deepseek-ai/DeepSeek-V3.2"},
		BaseURL:        "https://router.huggingface.co/v1",
	}}
}
