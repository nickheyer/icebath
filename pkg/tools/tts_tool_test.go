package tools

import "testing"

// Every platform that delivers native voice bubbles only for Ogg Opus is in the set
func TestOpusPlatformSet(t *testing.T) {
	t.Run("test_opus_platforms_cover_voice_bubble_platforms", func(t *testing.T) {
		for _, platform := range []string{"telegram", "matrix", "feishu", "whatsapp", "signal"} {
			if !OpusVoicePlatforms[platform] {
				t.Errorf("%s missing", platform)
			}
		}
		if len(OpusVoicePlatforms) != 5 {
			t.Fatalf("unexpected members %v", OpusVoicePlatforms)
		}
	})
}
