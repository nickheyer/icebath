package connections

import "syscall"

// Node code names of the Winsock errors a loopback listen fails with
var listenErrnoCodes = map[syscall.Errno]string{
	10048: "EADDRINUSE", 10049: "EADDRNOTAVAIL", 10013: "EACCES", 10024: "EMFILE", 10055: "ENOBUFS",
	10047: "EAFNOSUPPORT", 10022: "EINVAL",
}
