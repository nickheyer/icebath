package tuigateway

// What a slash completion row names, a built in command or a skill
type CompletionKind string

func (CompletionKind) Values() []string { return []string{"command", "skill"} }

// One popover row of the composer, its text put in from the replace offset, kind only on slash rows
type CompletionItem struct {
	Text    string          `json:"text"`
	Display string          `json:"display,omitzero"`
	Meta    string          `json:"meta,omitzero"`
	Kind    *CompletionKind `json:"kind,omitzero"`
}

type CompletionItemsResult struct {
	Items []CompletionItem `json:"items"`
}

// Token under the cursor, an at sign starting a context reference, and the folder or session the listing resolves against
type CompletePathParams struct {
	Profile   *string `json:"profile,omitzero"`
	Word      *string `json:"word,omitzero"`
	Cwd       *string `json:"cwd,omitzero"`
	SessionID *string `json:"session_id,omitzero"`
}

// Slash text to complete, the session binding skills to its workspace or the profile of a draft without one
type CompleteSlashParams struct {
	Text      *string `json:"text,omitzero"`
	SessionID *string `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// Ranked rows and the column an accepted row replaces from
type CompleteSlashResult struct {
	Items       []CompletionItem `json:"items"`
	ReplaceFrom *int64           `json:"replace_from,omitzero"`
}

// Files, folders, profiles and plugin references completing the composer word
var CompletePath = NewMethod[CompletePathParams, CompletionItemsResult]("complete.path")

// Ranked slash commands and skills completing a slash token
var CompleteSlash = NewMethod[CompleteSlashParams, CompleteSlashResult]("complete.slash")
