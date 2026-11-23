package tuiserver

// Asset extensions with their mime, in on disk lookup order
var assetExts = [][2]string{{"png", "image/png"}, {"jpg", "image/jpeg"}, {"webp", "image/webp"}}

var profilesRegistry = &handlerRegistry{}

func init() {
	profilesRegistry.method("profiles.create", profilesCreate)
	profilesRegistry.install()
}
