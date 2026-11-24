package tuiserver

var profilesRegistry = &handlerRegistry{}

func init() {
	profilesRegistry.method("profiles.create", profilesCreate)
	profilesRegistry.install()
}
