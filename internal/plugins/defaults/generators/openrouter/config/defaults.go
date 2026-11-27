package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: generationconfig.ImageKey,
		Title:  "OpenRouter images",
		About:  "Pictures OpenRouter draws, through its Image API or a chat request that answers with a picture, with the OpenRouter key connected in /providers",
		Settings: []config.Setting{
			{Key: Model, Title: "OpenRouter model", About: "Model OpenRouter draws with. Empty uses Image model, else OpenRouter's own pick", Default: ""},
			{Key: AspectRatio, Title: "OpenRouter aspect ratio", About: "Exact aspect ratio to ask OpenRouter for, such as 16:9. Empty picks it from the request", Default: ""},
			{Key: Surface, Title: "OpenRouter endpoint", About: "Which OpenRouter endpoint image requests go to", Default: "",
				Options: config.Options(config.Option{Value: "", Title: "Automatic", About: "Picked per model"},
					config.Option{Value: "images", Title: "Images", About: "The image generation endpoint"},
					config.Option{Value: "chat", Title: "Chat", About: "A chat request that returns an image"})},
			{Key: Timeout, Title: "OpenRouter time limit", About: "Seconds before an OpenRouter image request is given up, at least 1", Default: 5 * time.Minute,
				Min: config.Bound(time.Second)},
			{Key: Resolution, Title: "OpenRouter resolution", About: "Picture resolution to ask OpenRouter for, such as 1K or 2K. Empty takes the model's default", Default: ""},
			{Key: Quality, Title: "OpenRouter quality", About: "Picture quality to ask OpenRouter for, such as low or high. Empty takes the model's default", Default: ""},
			{Key: Background, Title: "OpenRouter background", About: "Background to ask OpenRouter for, such as transparent. Empty takes the model's default", Default: ""},
			{Key: OutputFormat, Title: "OpenRouter file type", About: "Picture file type to ask OpenRouter for, such as png or webp. Empty takes the model's default", Default: ""},
			{Key: OutputCompression, Title: "OpenRouter compression", About: "How much OpenRouter compresses jpeg and webp pictures, 0 to 100; -1 takes the model's default", Default: Unset},
			{Key: Seed, Title: "OpenRouter seed", About: "Fixed seed so OpenRouter draws the same picture again; -1 for a new seed each time", Default: Unset},
			{Key: Count, Title: "OpenRouter picture count", About: "How many pictures one OpenRouter request draws, at least 1", Default: 1},
		},
	}})
}
