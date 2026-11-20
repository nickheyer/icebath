package telegram

const platformHint = "You are on Telegram. Standard Markdown auto-converts: **bold**, " +
	"*italic*, ~~strikethrough~~, ||spoiler||, `code`, ```blocks```, " +
	"[links](url), ## headers. Prefer bullets or labeled lines for structured data (no tables). " +
	"You can send files natively: write MEDIA:/absolute/path/to/file in your response. " +
	"Images (.png, .jpg, .webp) send as photos, videos (.mp4) play inline; image URLs via ![alt](url) send as " +
	"photos. Audio: add [[audio_as_voice]] on its own line to send ANY audio file as a native voice bubble " +
	"(non-Opus transcodes automatically); without it, .mp3/.m4a arrive as audio files, other formats as documents."
