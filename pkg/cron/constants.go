package cron

const (
	// FireClaimTTLSeconds bounds how long an external fire claim blocks another fire
	FireClaimTTLSeconds = 300
	// FireClaimSkewSeconds is the clock skew tolerated between replicas claiming one occurrence
	FireClaimSkewSeconds = 60
	// ClaimTTLInactivityHeadroom multiplies the inactivity timeout into a run claim TTL
	ClaimTTLInactivityHeadroom = 3
)

// Reports a cron or interval job as opposed to a one shot, a missing schedule counting as not recurring
func isRecurring(job *Job) bool {
	return job != nil && job.Schedule != nil && (job.Schedule.Kind == "cron" || job.Schedule.Kind == "interval")
}
