package config

import "github.com/nickheyer/icebath/internal/config"

const (
	Name         = "vertex"
	Title        = "Google Vertex AI"
	BaseURL      = "https://aiplatform.googleapis.com"
	DefaultModel = "google/gemini-3.6-flash"
)

// Fields of an account, below its name
const CredentialsPath = "credentials_path"

// Field of an account that names its service account key: the file its sign-in reads, which the account keeps by its whole path
var KeyFile = config.Setting{Key: CredentialsPath, Title: "Service account key", About: "The JSON key file of a Google service account.",
	Help: "A path such as ~/keys/vertex.json.", Default: "", Path: &config.Path{Kind: config.PathFile, Exists: true}}

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
