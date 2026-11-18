package gateway

// Video extensions in declaration order
var SupportedVideoTypeOrder = videoTypeOrder

// Image document extensions in declaration order, the first key winning a reversed MIME lookup
var SupportedImageDocumentTypeOrder = imageDocumentTypeOrder

// Whether the URL points at an animated GIF
func IsAnimationURL(rawURL string) bool { return isAnimationURL(rawURL) }
