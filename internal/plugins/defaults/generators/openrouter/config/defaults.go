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
		Title:  "OpenRouter",
		About:  "Many image models through one OpenRouter account",
		Order:  40,
		Settings: []config.Setting{
			{Key: Model, Title: "Model", About: "The OpenRouter model that makes images.", Help: "When neither model is set, OpenRouter's own pick is used.",
				Default: "", Zero: "The general model"},
			{Key: AspectRatio, Title: "Aspect ratio", About: "The exact shape of image to ask for.", Help: "Such as 16:9.", Default: "", Zero: "The agent's choice"},
			{Key: Resolution, Title: "Resolution", About: "The resolution of image to ask for.", Help: "Such as 1K or 2K.", Default: "", Zero: "The model's default"},
			{Key: Quality, Title: "Quality", About: "The image quality to ask for.", Help: "Such as low or high.", Default: "", Zero: "The model's default"},
			{Key: Background, Title: "Background", About: "The background to ask for.", Help: "Such as transparent.", Default: "", Zero: "The model's default"},
			{Key: OutputFormat, Title: "File type", About: "The image file type to ask for.", Help: "Such as png or webp.", Default: "", Zero: "The model's default"},
			{Key: Count, Title: "Images per request", About: "How many images one request makes.", Default: 1, Counts: "images", Least: config.Num(1)},
			{Key: Surface, Title: "Request type", About: "Which kind of OpenRouter request makes images.", Default: "",
				Options: config.Options(
					config.Option{Value: "", Title: "Automatic", About: "icebath picks the kind for each model"},
					config.Option{Value: "images", Title: "Image request", About: "A request to the OpenRouter image service"},
					config.Option{Value: "chat", Title: "Chat request", About: "A chat request that answers with an image"},
				), Advanced: true},
			{Key: Timeout, Title: "Time limit", About: "How long one image request may take before it is given up.", Default: 5 * time.Minute,
				Min: config.Bound(time.Second), Advanced: true},
			{Key: OutputCompression, Title: "Compression", About: "The compression of jpeg and webp images.", Help: "100 compresses the most.", Default: Unset,
				Zero: "The model's default", ZeroValue: Unset, Least: config.Num(0), Most: config.Num(100), Advanced: true},
			{Key: Seed, Title: "Seed", About: "The fixed seed that makes the same image again.", Default: Unset, Zero: "New each time", ZeroValue: Unset,
				Counts: config.Plain, Advanced: true},
		},
	}})
}
