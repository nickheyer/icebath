package proc

// NetBSD kern.file2 node and the kinfo_file offsets of ki_ftype, ki_vtype and ki_fd
const (
	kinfoFileNode = "kern.file2"
	kinfoFTypeOff = 16
	kinfoVTypeOff = 88
	kinfoFDOff    = 108
	kinfoFileSize = 120
)

// NetBSD reports every pid 0 failure the way psutil raises it
const pidZeroListsNothing = false
