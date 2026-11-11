package gateway

import "github.com/nickheyer/icebath/pkg/tuigateway"

func init() { tuigateway.BindIdentity(IsAuthenticatedIdentity, HostOwnerSubject) }
