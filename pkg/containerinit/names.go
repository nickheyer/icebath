package containerinit

// Verb, seed variable and init binary name the image layout, the product binary and the bootstrap share
const (
	StageVerb      = "container-stage"
	RebootstrapEnv = "ICEBATH_AUTH_JSON_REBOOTSTRAP"
	InitName       = "icebath-init"
	productName    = "icebath"
)
