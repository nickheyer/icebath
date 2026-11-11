package dashboard

import "syscall"

// Windows has neither O_NOFOLLOW nor O_NONBLOCK, the post open identity check guards the entry
const desktopPluginOpenFlags = syscall.O_RDONLY
