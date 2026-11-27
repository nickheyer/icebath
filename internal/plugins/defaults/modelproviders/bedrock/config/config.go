package config

const (
	Name         = "bedrock"
	Title        = "AWS Bedrock"
	BaseURL      = "https://bedrock-runtime.us-east-1.amazonaws.com"
	DefaultModel = ""
)

const (
	APIKeyEnv  = "AWS_BEARER_TOKEN_BEDROCK"
	BaseURLEnv = "BEDROCK_BASE_URL"
)

// Settings of a provider, below its name
const (
	// Region requests are signed for and sent to
	Region = "region"
	// Profile of the AWS shared config the provider signs in with
	Profile = "aws_profile"
	// Guardrail applied to the replies: its id and version, both needed, whether it checks streamed text before it is shown, and how much
	// of why it acted it reports
	Guardrail          = "guardrail_identifier"
	GuardrailVersion   = "guardrail_version"
	GuardrailStreaming = "guardrail_stream_processing_mode"
	GuardrailTrace     = "guardrail_trace"
)
