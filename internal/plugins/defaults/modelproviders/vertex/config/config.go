package config

const (
	Name         = "vertex"
	Title        = "Google Vertex AI"
	BaseURL      = "https://aiplatform.googleapis.com"
	DefaultModel = "google/gemini-3.6-flash"
)

// Fields of an account, below its name
const CredentialsPath = "credentials_path"

// Settings of a provider, below its name
const (
	Project = "project_id"
	Region  = "region"
)

const DefaultRegion = "global"

const (
	CredentialsPathEnv = "VERTEX_CREDENTIALS_PATH"
	ProjectEnv         = "VERTEX_PROJECT_ID"
	RegionEnv          = "VERTEX_REGION"
)
