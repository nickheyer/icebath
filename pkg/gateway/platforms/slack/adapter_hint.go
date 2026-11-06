package slack

const platformHint = "You are in a Slack workspace communicating with your user. Standard markdown is auto-converted to Slack " +
	"formatting (bold, headers, links, code); tables are NOT supported — use bullet lists or labeled lines. You " +
	"can send media files natively: include MEDIA:/absolute/path/to/file in your response. Images (.png, .jpg, " +
	".webp) are uploaded as photo attachments, audio as file attachments. You can also include image URLs in " +
	"markdown format ![alt](url) and they will be uploaded as attachments."
