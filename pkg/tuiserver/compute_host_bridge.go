package tuiserver

import "github.com/nickheyer/icebath/pkg/tuigateway"

// Compute host metadata mirror of a session, empty when it holds none
func metadataMirror(session *tuigateway.Session) map[string]any {
	mirror, _ := session.Get("_metadata_mirror").(map[string]any)
	if mirror == nil {
		return map[string]any{}
	}
	return mirror
}
