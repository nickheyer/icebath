package gateway

import "github.com/nickheyer/icebath/pkg/tuihost"

func init() { tuihost.BindIdentity(IsAuthenticatedIdentity, HostOwnerSubject) }
