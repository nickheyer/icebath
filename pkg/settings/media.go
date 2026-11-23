package settings

import "time"

// Service the agent draws pictures with
var ImageGenProvider = Register(&Enum{Meta: Meta{Key: "image_gen.provider", Label: "Image generation",
	Short: "Which service draws pictures when the agent makes an image.",
	Group: GroupMedia, Order: 10},
	Open: true,
	Values: []Choice{
		{"", "Automatic", "The first service with a key."},
		{"fal", "FAL", ""}, {"openai", "OpenAI", ""}, {"openai-codex", "OpenAI via Codex", "Uses your Codex login."},
		{"xai", "xAI", ""}, {"openrouter", "OpenRouter", ""}, {"deepinfra", "DeepInfra", ""}, {"krea", "Krea", ""},
		{"meta-ai", "Meta AI", ""},
	}})

// Image model of the image service
var ImageGenModel = Register(&String{Meta: Meta{Key: "image_gen.model", Label: "Image model",
	Short: "Model the image service draws with.",
	Help:  "Empty uses the service's default.",
	Group: GroupMedia, Order: 11}})

// How pictures you attach reach the model
var ImageInput = Register(&Enum{Meta: Meta{Key: "agent.image_input_mode", Label: "Image input",
	Short: "How pictures you attach reach the model.",
	Group: GroupMedia, Order: 20, Apply: NextTurn},
	Default: "auto",
	Values: []Choice{
		{"auto", "Automatic", "Send the picture when the model can see."},
		{"native", "Send picture", "Always send the picture itself."},
		{"text", "Describe it", "A helper model describes it in words."},
	}})

// Pictures drawn inside the terminal
var InlineImages = Register(&Enum{Meta: Meta{Key: "display.inline_images", Label: "Show images inline",
	Short: "Draw pictures inside the transcript instead of as links.",
	Group: GroupMedia, Order: 30},
	Default: "auto", On: "auto", Off: "off",
	Values: []Choice{
		{"auto", "Automatic", "Best method this terminal supports."},
		{"kitty", "Graphics", "Full pictures in terminals that support them."},
		{"unicode", "Blocks", "Coloured text blocks, works everywhere."},
		{"off", "Off", "No pictures."},
	}})

// Image model OpenRouter draws with
var OpenRouterImageModel = Register(&String{Meta: Meta{Key: "image_gen.openrouter.model", Label: "OpenRouter model",
	Short: "Model OpenRouter draws with.",
	Help:  "Empty uses Image model, else OpenRouter's own pick.",
	Group: GroupMedia, Order: 100, Advanced: true}})

// Exact aspect ratio sent to OpenRouter
var OpenRouterImageAspect = Register(&String{Meta: Meta{Key: "image_gen.openrouter.aspect_ratio", Label: "OpenRouter aspect ratio",
	Short: "Exact aspect ratio to ask OpenRouter for, such as 16:9.",
	Group: GroupMedia, Order: 101, Advanced: true},
	Null: "picked from the request"})

// OpenRouter endpoint image requests use
var OpenRouterImageSurface = Register(&Enum{Meta: Meta{Key: "image_gen.openrouter.surface", Label: "OpenRouter endpoint",
	Short: "Which OpenRouter endpoint image requests go to.",
	Group: GroupMedia, Order: 102, Advanced: true},
	Null: "picked per model",
	Values: []Choice{
		{"images", "Images", "The image generation endpoint."},
		{"chat", "Chat", "A chat request that returns an image."},
	}})

// Time limit of one OpenRouter image request
var OpenRouterImageTimeout = Register(&Duration{Meta: Meta{Key: "image_gen.openrouter.timeout", Label: "OpenRouter time limit",
	Short: "Give up on an OpenRouter image request after this long.",
	Group: GroupMedia, Order: 103, Advanced: true},
	Null: "5 minutes", Min: at(time.Second)})
