//go:build !icebath_core

package gatewayrun

import "github.com/nickheyer/icebath/pkg/media"

// Binds inbound voice transcription to the media speech to text backends
func init() {
	transcribeVoice = media.TranscribeAudio
	transcribeVoiceFallback = media.TranscribeAudioLocalFallback
}
