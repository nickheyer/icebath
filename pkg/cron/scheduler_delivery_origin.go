package cron

// Stamps the origin scope id and the relay user id onto a live send metadata without overriding
func stampOriginDiscriminators(t *targetDelivery, routeMeta, mediaMeta map[string]any) {
	var scope, user any
	if t.originTarget {
		scope = t.origin["scope_id"]
	}
	if t.isRelay() && t.originUserID != nil {
		user = *t.originUserID
	}
	for _, kv := range []struct {
		key   string
		value any
	}{{"scope_id", scope}, {"user_id", user}} {
		if !pyTruthy(kv.value) {
			continue
		}
		text := optionalStr(kv.value)
		if _, ok := routeMeta[kv.key]; !ok {
			routeMeta[kv.key] = text
		}
		if _, ok := mediaMeta[kv.key]; !ok {
			mediaMeta[kv.key] = text
		}
	}
}
