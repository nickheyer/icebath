package gateway

import "github.com/nickheyer/icebath/pkg/plugins"

// Host rendezvous reads the plugin activation path makes to reach the running serve backend
type hostRendezvousService struct{}

// Rendezvous record a plugins.HostRecord carries, rebuilt from its role and port when it came from elsewhere
func rendezvousRecord(record *plugins.HostRecord) *HostRecord {
	if r, ok := record.Record.(*HostRecord); ok {
		return r
	}
	port := record.Port
	return &HostRecord{Role: record.Role, Port: &port}
}

func (hostRendezvousService) ReadRecord(role string) (*plugins.HostRecord, error) {
	r := ReadHostRecord(role, false)
	if r == nil {
		return nil, nil
	}
	port := 0
	if r.Port != nil {
		port = *r.Port
	}
	return &plugins.HostRecord{Role: r.Role, Port: port, Record: r}, nil
}

func (hostRendezvousService) RecordTokenIsConsistent(record *plugins.HostRecord) bool {
	return HostRecordTokenIsConsistent(rendezvousRecord(record))
}

func (hostRendezvousService) ReadToken(role string) (string, error) {
	return ReadHostToken(role), nil
}

func (hostRendezvousService) DialHost(record *plugins.HostRecord) string {
	return HostDialHost(rendezvousRecord(record))
}

// Host rendezvous the plugin services expose for serve backend discovery
func HostRendezvousService() plugins.HostRendezvous {
	return hostRendezvousService{}
}
