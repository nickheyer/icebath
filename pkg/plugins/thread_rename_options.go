package plugins

// Keyword options of an adapter thread rename, the native guard or the relay connector guard
type ThreadRenameOptions struct {
	OnlyIfCurrentName      *string
	PreferConnectorCreated bool
	ParentChatID           *string
}
