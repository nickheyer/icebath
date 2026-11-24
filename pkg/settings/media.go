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

// Pictures drawn at the same time when the agent asks for several
var ImageGenMaxParallelRequests = Register(&Int{Meta: Meta{Key: "image_gen.max_parallel_requests", Label: "Pictures at once",
	Short: "Pictures drawn at the same time when the agent asks for several.",
	Group: GroupMedia, Order: 12, Advanced: true},
	Default: 4, Min: at(1), Max: at(8)})

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

// Picture resolution asked of OpenRouter
var OpenRouterImageResolution = Register(&String{Meta: Meta{Key: "image_gen.openrouter.resolution", Label: "OpenRouter resolution",
	Short: "Picture resolution to ask OpenRouter for, such as 1K or 2K.",
	Group: GroupMedia, Order: 104, Advanced: true},
	Null: "the model's default"})

// Picture quality asked of OpenRouter
var OpenRouterImageQuality = Register(&String{Meta: Meta{Key: "image_gen.openrouter.quality", Label: "OpenRouter quality",
	Short: "Picture quality to ask OpenRouter for, such as low or high.",
	Group: GroupMedia, Order: 105, Advanced: true},
	Null: "the model's default"})

// Picture background asked of OpenRouter
var OpenRouterImageBackground = Register(&String{Meta: Meta{Key: "image_gen.openrouter.background", Label: "OpenRouter background",
	Short: "Background to ask OpenRouter for, such as transparent.",
	Group: GroupMedia, Order: 106, Advanced: true},
	Null: "the model's default"})

// Picture file type asked of OpenRouter
var OpenRouterImageOutputFormat = Register(&String{Meta: Meta{Key: "image_gen.openrouter.output_format", Label: "OpenRouter file type",
	Short: "Picture file type to ask OpenRouter for, such as png or webp.",
	Group: GroupMedia, Order: 107, Advanced: true},
	Null: "the model's default"})

// Compression of jpeg and webp pictures OpenRouter draws
var OpenRouterImageOutputCompression = Register(&Int{Meta: Meta{Key: "image_gen.openrouter.output_compression", Label: "OpenRouter compression",
	Short: "How much OpenRouter compresses jpeg and webp pictures, 0 to 100.",
	Group: GroupMedia, Order: 108, Advanced: true},
	Null: "the model's default", Min: at(0), Max: at(100)})

// Fixed seed of OpenRouter pictures
var OpenRouterImageSeed = Register(&Int{Meta: Meta{Key: "image_gen.openrouter.seed", Label: "OpenRouter seed",
	Short: "Fixed seed so OpenRouter draws the same picture again.",
	Group: GroupMedia, Order: 109, Advanced: true},
	Null: "a new seed each time"})

// Pictures one OpenRouter request draws
var OpenRouterImageCount = Register(&Int{Meta: Meta{Key: "image_gen.openrouter.count", Label: "OpenRouter picture count",
	Short: "How many pictures one OpenRouter request draws.",
	Group: GroupMedia, Order: 110, Advanced: true},
	Null: "one picture", Min: at(1)})

// Image model OpenAI draws with
var OpenAIImageModel = Register(&String{Meta: Meta{Key: "image_gen.openai.model", Label: "OpenAI image model",
	Short: "Model OpenAI draws with, a tier name or any model id.",
	Help:  "Empty uses Image model, else GPT Image 2 at medium quality.",
	Group: GroupMedia, Order: 120, Advanced: true, Env: "OPENAI_IMAGE_MODEL"},
	Null: "Image model, else the default tier"})

// Custom endpoint OpenAI image requests go to
var OpenAIImageProvider = Register(&String{Meta: Meta{Key: "image_gen.openai.provider", Label: "OpenAI image endpoint",
	Short: "Endpoint under providers that OpenAI image requests go to.",
	Group: GroupMedia, Order: 121, Advanced: true},
	Null: "the OpenAI endpoint"})

// Base URL OpenAI image requests go to
var OpenAIImageBaseURL = Register(&String{Meta: Meta{Key: "image_gen.openai.base_url", Label: "OpenAI image address",
	Short: "Base URL OpenAI image requests go to.",
	Help:  "Wins over OpenAI image endpoint.",
	Group: GroupMedia, Order: 122, Advanced: true},
	Null: "the endpoint's address, else OPENAI_BASE_URL"})

// Key OpenAI image requests send
var OpenAIImageKeyEnv = Register(&String{Meta: Meta{Key: "image_gen.openai.key_env", Label: "OpenAI image key",
	Short: "Name of the key in your keys that OpenAI image requests send.",
	Group: GroupMedia, Order: 123, Advanced: true},
	Null: "the endpoint's key, else OPENAI_API_KEY"})

// Quality tier of images drawn through the Codex login
var OpenAICodexImageModel = Register(&String{Meta: Meta{Key: "image_gen.openai-codex.model", Label: "Codex image tier",
	Short: "Quality tier of pictures drawn through your Codex login.",
	Group: GroupMedia, Order: 130, Advanced: true, Env: "OPENAI_IMAGE_MODEL"},
	Null: "Image model, else the default tier"})

// Image model xAI draws with
var XAIImageModel = Register(&String{Meta: Meta{Key: "image_gen.xai.model", Label: "xAI image model",
	Short: "Model xAI draws with.",
	Group: GroupMedia, Order: 140, Advanced: true, Env: "XAI_IMAGE_MODEL"},
	Null: "xAI's default image model"})

// Picture size xAI draws at
var XAIImageResolution = Register(&Enum{Meta: Meta{Key: "image_gen.xai.resolution", Label: "xAI resolution",
	Short: "Picture size xAI draws at.",
	Group: GroupMedia, Order: 141, Advanced: true},
	Default: "1k",
	Values:  []Choice{{"1k", "1K", "About one megapixel."}, {"2k", "2K", "About four megapixels."}}})

// Pictures xAI draws kept in xAI storage
var XAIImageStorage = Register(&Bool{Meta: Meta{Key: "image_gen.xai.storage.enabled", Label: "xAI image storage",
	Short: "Keep pictures xAI draws in xAI storage for a reusable link.",
	Help:  "xAI may bill for stored files.",
	Group: GroupMedia, Order: 142, Advanced: true},
	Default: true})

// Public link of pictures kept in xAI storage
var XAIImagePublicURL = Register(&Bool{Meta: Meta{Key: "image_gen.xai.storage.public_url", Label: "xAI image link",
	Short: "Give pictures kept in xAI storage a public link.",
	Group: GroupMedia, Order: 143, Advanced: true},
	Default: true})

// How long xAI keeps stored pictures
var XAIImageExpiresAfter = Register(&String{Meta: Meta{Key: "image_gen.xai.storage.expires_after", Label: "xAI image expiry",
	Short: "Seconds xAI keeps stored pictures, at most 30 days.",
	Help:  "Never, permanent or 0 keeps them until you delete them.",
	Group: GroupMedia, Order: 144, Advanced: true},
	Null: "kept until you delete them"})

// Image model DeepInfra draws with
var DeepInfraImageModel = Register(&String{Meta: Meta{Key: "image_gen.deepinfra.model", Label: "DeepInfra image model",
	Short: "Model DeepInfra draws with.",
	Group: GroupMedia, Order: 150, Advanced: true, Env: "DEEPINFRA_IMAGE_MODEL"},
	Null: "the first in DeepInfra's catalog"})

// Custom endpoint of DeepInfra image requests
var DeepInfraImageBaseURL = Register(&String{Meta: Meta{Key: "image_gen.deepinfra.base_url", Label: "DeepInfra image address",
	Short: "Custom endpoint for DeepInfra image requests.",
	Group: GroupMedia, Order: 151, Advanced: true},
	Null: "the provider endpoint"})

// Image model Krea draws with
var KreaImageModel = Register(&String{Meta: Meta{Key: "image_gen.krea.model", Label: "Krea model",
	Short: "Model Krea draws with.",
	Group: GroupMedia, Order: 160, Advanced: true, Env: "KREA_IMAGE_MODEL"},
	Null: "Image model, else Krea's default"})

// How freely Krea 2 reads a prompt
var KreaImageCreativity = Register(&Enum{Meta: Meta{Key: "image_gen.krea.creativity", Label: "Krea creativity",
	Short: "How freely Krea 2 reads your prompt.",
	Group: GroupMedia, Order: 161, Advanced: true},
	Default: "medium",
	Values:  []Choice{{"raw", "Raw", "Your prompt as written."}, {"low", "Low", ""}, {"medium", "Medium", ""}, {"high", "High", "The most freedom."}}})

// Krea enlarging each picture it draws
var KreaImageUpscale = Register(&Bool{Meta: Meta{Key: "image_gen.krea.upscale", Label: "Krea upscale",
	Short: "Have Krea enlarge each picture it draws.",
	Group: GroupMedia, Order: 162, Advanced: true},
	Null: "the model's default"})

// Image model Meta AI draws with
var MetaAIImageModel = Register(&String{Meta: Meta{Key: "image_gen.meta-ai.model", Label: "Meta AI model",
	Short: "Model Meta AI draws with.",
	Group: GroupMedia, Order: 170, Advanced: true, Env: "META_IMAGE_MODEL"},
	Null: "Image model, else Meta AI's default"})
