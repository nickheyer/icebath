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
