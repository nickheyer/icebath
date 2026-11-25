package home

import "github.com/nickheyer/icebath/pkg/utils/netpref"

// Makes the process resolve unspecified address families as IPv4 first, the network.force_ipv4 setting
func ApplyIPv4Preference(force bool) { netpref.ApplyIPv4Preference(force) }
