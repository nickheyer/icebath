package googlegenerativeai

import "encoding/json"

type request struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []tool            `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

// One Part with its data union and the thought metadata that may ride on any kind
type part struct {
	Text                *string           `json:"text,omitempty"`
	InlineData          *blob             `json:"inlineData,omitempty"`
	FunctionCall        *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse    *functionResponse `json:"functionResponse,omitempty"`
	FileData            json.RawMessage   `json:"fileData,omitempty"`
	ExecutableCode      json.RawMessage   `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage   `json:"codeExecutionResult,omitempty"`
	ToolCall            json.RawMessage   `json:"toolCall,omitempty"`
	ToolResponse        json.RawMessage   `json:"toolResponse,omitempty"`
	Thought             bool              `json:"thought,omitempty"`
	ThoughtSignature    string            `json:"thoughtSignature,omitempty"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	ID       string                 `json:"id,omitempty"`
	Name     string                 `json:"name"`
	Response map[string]string      `json:"response"`
	Parts    []functionResponsePart `json:"parts,omitempty"`
}

type functionResponsePart struct {
	InlineData blob `json:"inlineData"`
}

type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
}

type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type generationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

// GenerateContentResponse, also the shape of every streamed chunk
type response struct {
	Candidates     []candidate     `json:"candidates"`
	PromptFeedback *promptFeedback `json:"promptFeedback"`
	UsageMetadata  *usageMetadata  `json:"usageMetadata"`
	ModelVersion   string          `json:"modelVersion"`
	ResponseID     string          `json:"responseId"`
	Error          *apiError       `json:"error"`
}

// Candidate with the rating and scoring fields the decoder accepts without acting on
type candidate struct {
	Content           content            `json:"content"`
	FinishReason      string             `json:"finishReason"`
	FinishMessage     string             `json:"finishMessage"`
	SafetyRatings     json.RawMessage    `json:"safetyRatings"`
	AvgLogprobs       float64            `json:"avgLogprobs"`
	CitationMetadata  *citationMetadata  `json:"citationMetadata"`
	GroundingMetadata *groundingMetadata `json:"groundingMetadata"`
}

// Citation list under the Gemini API name and the Vertex name
type citationMetadata struct {
	CitationSources []citationSource `json:"citationSources"`
	Citations       []citationSource `json:"citations"`
}

type citationSource struct {
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	URI        string `json:"uri"`
	Title      string `json:"title"`
	License    string `json:"license"`
}

type groundingMetadata struct {
	GroundingChunks   []groundingChunk   `json:"groundingChunks"`
	GroundingSupports []groundingSupport `json:"groundingSupports"`
}

type groundingChunk struct {
	Web              *groundingSource `json:"web"`
	RetrievedContext *groundingSource `json:"retrievedContext"`
}

type groundingSource struct {
	URI   string `json:"uri"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type groundingSupport struct {
	Segment               *segment `json:"segment"`
	GroundingChunkIndices []int    `json:"groundingChunkIndices"`
}

type segment struct {
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	Text       string `json:"text"`
}

type promptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
}

// google.rpc.Status as mapped to HTTP JSON by AIP-193
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

type errorEnvelope struct {
	Error *apiError `json:"error"`
}
