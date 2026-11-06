package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// The single gateway process on this host with the profile roster it multiplexes
type HostGatewayTopology struct {
	PID      int
	Profiles []string
	Source   string
	Home     string
}

func normalizedProfile(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	return profiles.NormalizeProfileName(name)
}

// Whether this host process serves the profile, default included
func (t *HostGatewayTopology) Serves(profileName string) (bool, error) {
	wanted, err := normalizedProfile(profileName)
	if err != nil || wanted == "" {
		return false, err
	}
	for _, p := range t.Profiles {
		n, err := normalizedProfile(p)
		if err != nil {
			return false, err
		}
		if n == wanted {
			return true, nil
		}
	}
	return false, nil
}

// Shared phrasing naming the host gateway pid and the profiles it serves
func (t *HostGatewayTopology) Describe() string {
	roster := "an unknown profile set"
	if len(t.Profiles) > 0 {
		roster = strings.Join(t.Profiles, ", ")
	}
	return fmt.Sprintf("the host gateway (PID %d) serving profiles %s", t.PID, roster)
}

// Whether the owner was launched from another icebath root, a legacy record without a home counting as ours
func launchedByOtherTenant(ownerHome, ourHome string) (bool, error) {
	if ownerHome == "" {
		return false, nil
	}
	same, err := sameHermesHome(home.RootOf(ownerHome), home.RootOf(ourHome))
	return !same, err
}

// Topology from the host record when it positively names a live owner of this tenant
func topologyFromHostRecord() (*HostGatewayTopology, error) {
	record, err := readHostRecord(roleGateway, false)
	if err != nil || record == nil || record.PID == 0 {
		return nil, err
	}
	if record.CreateTime == nil || !recordNamesLiveOwner(record) {
		return nil, nil
	}
	other, err := launchedByOtherTenant(record.Home, home.Dir(context.Background()))
	if err != nil || other {
		return nil, err
	}
	return &HostGatewayTopology{PID: record.PID, Profiles: append([]string{}, record.Profiles...), Source: "host_record",
		Home: hostRecordHome(record)}, nil
}

// Topology from the default home's served_profiles behind a proven live pid, for gateways predating the record
func topologyFromServedRecord() (*HostGatewayTopology, error) {
	pid, live, err := LiveDefaultGatewayPid()
	if err != nil || !live {
		return nil, err
	}
	served, _, err := RecordedServedProfiles("")
	if err != nil {
		return nil, err
	}
	roster := []string{"default"}
	for _, p := range served {
		n, err := normalizedProfile(p)
		if err != nil {
			return nil, err
		}
		if n != "default" {
			roster = append(roster, p)
		}
	}
	return &HostGatewayTopology{PID: pid, Profiles: roster, Source: "served_record"}, nil
}

// The one live host gateway and its served profiles, nil when no gateway owns the role
func hostGatewayTopology() (*HostGatewayTopology, error) {
	for _, rung := range []func() (*HostGatewayTopology, error){topologyFromHostRecord, topologyFromServedRecord} {
		topology, err := rung()
		if err != nil || topology != nil {
			return topology, err
		}
	}
	return nil, nil
}

// Host gateway when it serves the profile, an empty name meaning the active profile, nil otherwise
func HostGatewayServing(ctx context.Context, profileName string) (*HostGatewayTopology, error) {
	topology, err := hostGatewayTopology()
	if err != nil || topology == nil {
		return nil, err
	}
	if profileName == "" {
		profileName = profiles.GetActiveProfileName(ctx)
	}
	serves, err := topology.Serves(profileName)
	if err != nil || !serves {
		return nil, err
	}
	return topology, nil
}
