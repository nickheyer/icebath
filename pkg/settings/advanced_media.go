package settings

// Where files you attach to a prompt are saved
var AttachmentsStorage = Register(&Enum{Meta: Meta{Key: "attachments.storage", Label: "Attachment storage", Short: "Where files you attach to a prompt are saved.", Group: GroupMedia, Order: 1000, Advanced: true}, Default: "icebath-home", Values: []Choice{{"icebath-home", "Icebath folder", "Keep attachments in the Icebath home folder."}, {"workspace", "Project folder", "Keep attachments inside the project."}}})

// Size images are shrunk to when kept in chat history
var VisionEmbedTargetBytes = Register(&Int{Meta: Meta{Key: "vision.embed_target_bytes", Label: "Image size budget", Short: "Size images are shrunk to when kept in chat history.", Group: GroupMedia, Order: 1010, Advanced: true}, Default: 262144, Unit: "bytes", Min: at(65536), Max: at(4194304)})

// Most times the same image may be analyzed in one chat
var VisionMaxCallsPerImage = Register(&Int{Meta: Meta{Key: "vision.max_calls_per_image", Label: "Looks per image", Short: "Most times the same image may be analyzed in one chat.", Help: "Zero means no limit.", Group: GroupMedia, Order: 1020, Advanced: true}, Null: "no limit", Min: at(0)})

// How many earlier images are shown to the model again
var VisionReplayRecentImages = Register(&Int{Meta: Meta{Key: "vision.replay_recent_images", Label: "Replayed images", Short: "How many earlier images are shown to the model again.", Help: "Zero stops earlier images from being sent again.", Group: GroupMedia, Order: 1030, Advanced: true}, Default: 3, Min: at(0)})

// Video generation provider plugin
var VideoGenProvider = Register(&String{Meta: Meta{Key: "video_gen.provider", Label: "Video generation", Short: "Plugin that makes videos when the agent asks for one.", Group: GroupMedia, Order: 2990, Advanced: true}, Null: "the first available provider"})

// Video model the video service makes videos with
var VideoGenModel = Register(&String{Meta: Meta{Key: "video_gen.model", Label: "Video model", Short: "Model the video service makes videos with.", Help: "Empty uses the service's default.", Group: GroupMedia, Order: 2991, Advanced: true}, Null: "the service's default"})

// Model family FAL makes videos with
var FalVideoModel = Register(&String{Meta: Meta{Key: "video_gen.fal.model", Label: "FAL video model", Short: "Model family FAL makes videos with.", Group: GroupMedia, Order: 2992, Advanced: true, Env: "FAL_VIDEO_MODEL"}, Null: "Video model, else FAL's default"})

// Videos xAI makes kept in xAI storage
var XAIVideoStorage = Register(&Bool{Meta: Meta{Key: "video_gen.xai.storage.enabled", Label: "xAI video storage", Short: "Keep videos xAI makes in xAI storage for a reusable link.", Help: "xAI may bill for stored files.", Group: GroupMedia, Order: 2993, Advanced: true}, Default: true})

// Public link of videos kept in xAI storage
var XAIVideoPublicURL = Register(&Bool{Meta: Meta{Key: "video_gen.xai.storage.public_url", Label: "xAI video link", Short: "Give videos kept in xAI storage a public link.", Group: GroupMedia, Order: 2994, Advanced: true}, Default: true})

// How long xAI keeps stored videos
var XAIVideoExpiresAfter = Register(&String{Meta: Meta{Key: "video_gen.xai.storage.expires_after", Label: "xAI video expiry", Short: "Seconds xAI keeps stored videos, at most 30 days.", Help: "Never, permanent or 0 keeps them until you delete them.", Group: GroupMedia, Order: 2995, Advanced: true}, Null: "kept until you delete them"})
