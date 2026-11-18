//go:build darwin

#include "textflag.h"

TEXT libc_sigaction_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sigaction(SB)
GLOBL	·libc_sigaction_trampoline_addr(SB), RODATA, $8
DATA	·libc_sigaction_trampoline_addr(SB)/8, $libc_sigaction_trampoline<>(SB)
