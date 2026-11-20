package tools

// Platforms whose native voice bubble delivery needs Ogg Opus because MP3 renders broken there
var OpusVoicePlatforms = map[string]bool{"telegram": true, "matrix": true, "feishu": true, "whatsapp": true, "signal": true}
