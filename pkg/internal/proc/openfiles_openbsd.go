package proc

// OpenBSD kern.file node and the kinfo_file offsets of f_type, v_type and fd_fd
const (
	kinfoFileNode = "kern.file"
	kinfoFTypeOff = 16
	kinfoVTypeOff = 120
	kinfoFDOff    = 380
	kinfoFileSize = 384
)

// OpenBSD answers an ESRCH for pid 0 that psutil turns into an empty list
const pidZeroListsNothing = true
