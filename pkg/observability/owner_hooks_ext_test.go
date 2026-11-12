package observability_test

// Links the packages whose init installs the observability hooks, as importing the hermes modules does
import (
	_ "github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/plugins"
	_ "github.com/nickheyer/icebath/pkg/skills"
)
