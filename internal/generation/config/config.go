package config

const (
	ImageKey      = "image_gen"
	ImageProvider = ImageKey + ".provider"
	ImageModel    = ImageKey + ".model"
	// Pictures drawn at the same time when the agent asks for several in one reply
	ImageParallel = ImageKey + ".max_parallel_requests"
	ImageDebug    = ImageKey + ".debug"

	VideoKey = "video_gen"
	// Whether video_generate is offered at all, as alpha's video_gen toolset was off until enabled
	VideoEnabled  = VideoKey + ".enabled"
	VideoProvider = VideoKey + ".provider"
	VideoModel    = VideoKey + ".model"
)

// Bounds of image_gen.max_parallel_requests
const (
	MinParallel = 1
	MaxParallel = 8
)
