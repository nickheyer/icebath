package state

import (
	"encoding/hex"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Hex characters an interactive surface mints after the timestamp
const DefaultSessionIDHexLen = 6

// Timestamp prefix every minted session id starts with
var sessionIDPattern = regexp.MustCompile(`^\p{Nd}{8}_\p{Nd}{6}_`)

// Shapes of every id a surface mints or derives, the salvage lane's layout sentinels
var sessionIDRecognizers = []*regexp.Regexp{
	sessionIDPattern,
	regexp.MustCompile(`^cron_[A-Za-z0-9][A-Za-z0-9._-]*_\p{Nd}{8}_\p{Nd}{6}\n?$`),
	regexp.MustCompile(`^bg_(?:\p{Nd}{6}_)?[0-9a-f]{6}\n?$`),
	regexp.MustCompile(`^room_[0-9a-f]{32}\n?$`),
	regexp.MustCompile(`^api_\p{Nd}{9,}_[0-9a-f]{8}\n?$`),
	regexp.MustCompile(`^api-[0-9a-f]{16}\n?$`),
	regexp.MustCompile(`^run_[0-9a-f]{32}\n?$`),
	regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\n?$`),
}

// Reports whether value has the shape of an id one of the surfaces really mints or derives
func IsKnownSessionID(value string) bool {
	for _, re := range sessionIDRecognizers {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

// Fresh session id of the local time now and hexLen random hex characters, the timestamp pinned to the given clock
func NewSessionID(now time.Time, hexLen int) (string, error) {
	u, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	random := hex.EncodeToString(u[:])
	return now.Local().Format("20060102_150405") + "_" + random[:min(max(hexLen, 0), len(random))], nil
}
