package googlemeet

func init() { registerGoogleMeetPlugin() }

var (
	_ = meetToolHandlers
	_ = registerMeetCLI
)
