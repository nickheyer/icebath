//go:build windows

package gateway

import "os"

// Subject a managed worker's adoption ticket carries, the owner's process SID
func ownerWorkerSubject() string {
	sid, err := nativePipeFacts.processSID(uint32(os.Getpid()))
	if err != nil {
		panic(err)
	}
	return "sid:" + sid
}
