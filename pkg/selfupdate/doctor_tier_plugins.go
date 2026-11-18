//go:build !icebath_core

package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/discord"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/matrix"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/media"
)

// Binds the doctor rows of Discord voice, FFmpeg, sherpa-onnx, Matrix E2EE and the WhatsApp session, and the local speech probes
func init() {
	setDoctorPackageProbe("Opus (Discord voice playback)", func(context.Context) bool { return discord.LoadOpusLibrary() == nil })
	setDoctorPackageProbe("FFmpeg (audio conversion)", func(context.Context) bool { return media.FindFFmpegBinary() != "" })
	setDoctorPackageProbe("sherpa-onnx (wake word)", func(context.Context) bool { return media.SherpaOnnxAvailable() })
	doctorMatrixE2EE = func() (bool, string) { return matrix.E2EEBuiltIn, matrix.E2EEBuildCommand }
	doctorWhatsAppSession = whatsAppSessionOfGatewayConfig
	doctorLiveSpeech = func(e *doctorLiveEnv) {
		e.speak = func(text, outputPath string) (string, error) { return media.TextToSpeechTool(e.ctx, text, outputPath) }
		e.transcribe = func(path string) (map[string]any, error) { return media.TranscribeAudio(e.ctx, path, "", "") }
		e.sample = func() (string, string, error) { return media.SpeechSample(e.ctx) }
	}
}

// Session directory, store presence and pairing of the in process WhatsApp adapter for the configured session path
func whatsAppSessionOfGatewayConfig(ctx context.Context) (string, bool, bool, error) {
	var extra map[string]any
	if cfg, err := gateway.LoadGatewayConfig(ctx); err == nil {
		if wa := cfg.Platforms[gateway.PlatformWhatsApp]; wa != nil {
			extra = wa.Extra
		}
	} else {
		return "", false, false, err
	}
	return whatsapp.SessionStatus(ctx, extra)
}
