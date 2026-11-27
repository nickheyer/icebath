// Microsoft Edge's read aloud voices
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/edge/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/edge/edgetts"
	"github.com/nickheyer/icebath/internal/speech"
)

// Longest an Edge synthesis may take
const deadline = 60 * time.Second

func Register(registry *speech.Registry) {
	client := &edgetts.Client{}
	registry.Register(speech.Services{Speakers: []speech.Voice{{
		Name: config.Name, Title: "Microsoft Edge TTS", About: "Good quality, no API key needed", Label: "Edge TTS",
		Setup:  speech.Setup{Badge: "★ recommended · free"},
		Limit:  func(*speech.Job) int { return config.DefaultLimit },
		Format: "mp3", Opus: speech.TranscodedOpus,
		Synthesize: func(ctx context.Context, job *speech.Job, request speech.Request) (string, error) {
			return synthesize(ctx, client, job, request)
		},
	}}})
}

// Edge rate of a speed, a signed whole percentage
func rate(speed float64) string {
	return fmt.Sprintf("%+d%%", int(math.RoundToEven((speed-1.0)*100)))
}

func synthesize(ctx context.Context, client *edgetts.Client, job *speech.Job, request speech.Request) (string, error) {
	speed, _ := job.Speed(request, config.Speed)
	options := edgetts.Options{Voice: job.Text(config.Voice, config.DefaultVoice)}
	if speed != 1.0 {
		options.Rate = rate(speed)
	}
	bounded, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var audio bytes.Buffer
	if err := client.Synthesize(bounded, request.Text, options, &audio); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return "", errors.New("Edge TTS did not finish within 60 seconds")
		}
		return "", err
	}
	return request.Path, os.WriteFile(request.Path, audio.Bytes(), 0o644)
}
