package config

const (
	ImageKey = "image_gen"
	// Service that makes images, the first that is ready when empty
	ImageProvider = ImageKey + ".provider"
	// Pictures drawn at the same time when the agent asks for several in one reply
	ImageParallel = ImageKey + ".max_parallel_requests"
	ImageDebug    = ImageKey + ".debug"

	VideoKey = "video_gen"
	// Whether the agent makes videos at all, the one control of it: video_generate is offered only while it is on
	VideoEnabled = VideoKey + ".enabled"
	// Service that makes videos while they are made, the first that is ready when empty
	VideoProvider = VideoKey + ".provider"
)

// Kinds of thing the model setting of a service names, as the pages of Images and Videos register their pickers: the models of the service
// named after the kind, such as image-model:fal
const (
	ImageModels = "image-model"
	VideoModels = "video-model"
)

// What the model setting of the image service with the name picks
func ImageModelOf(service string) string {
	return ImageModels + ":" + service
}

// What the model setting of the video service with the name picks
func VideoModelOf(service string) string {
	return VideoModels + ":" + service
}

// Bounds of image_gen.max_parallel_requests
const (
	MinParallel = 1
	MaxParallel = 8
)
