package dashboard

// API paths that bypass dashboard auth, shared by the loopback and the gated middleware
var PublicAPIPaths = map[string]bool{
	"/api/health":            true,
	"/api/status":            true,
	"/api/config/defaults":   true,
	"/api/config/schema":     true,
	"/api/model/info":        true,
	"/api/dashboard/themes":  true,
	"/api/dashboard/plugins": true,
	"/api/cron/fire":         true,
}
