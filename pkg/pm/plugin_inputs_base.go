package pm

// PluginInput is the one source of plugin members a sync or currency probe takes
type PluginInput interface{ pluginInput() }

// Candidates discovers the enabled members and adds these candidate directories
type Candidates struct{ Dirs []string }

// Selection publishes a proposed plugin enablement with the members it implies
type Selection struct{ Data map[string]any }

// StagedUpdate publishes a staged plugin tree over its install
type StagedUpdate struct{ Data map[string]any }

func (*Candidates) pluginInput()   {}
func (*Selection) pluginInput()    {}
func (*StagedUpdate) pluginInput() {}
