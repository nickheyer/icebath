//go:build arm64 && (darwin || linux || windows)

#include "textflag.h"

GLOBL ·x8callABI0(SB), NOPTR|RODATA, $8
DATA ·x8callABI0(SB)/8, $x8call(SB)

// Loads X8 and X0 to X3 from the frame in X0, calls the C function and returns with LR restored
TEXT x8call(SB), NOSPLIT|NOFRAME, $0
	SUB	$16, RSP
	MOVD	R30, 8(RSP)
	MOVD	R0, R9
	MOVD	8(R9), R8
	MOVD	16(R9), R0
	MOVD	24(R9), R1
	MOVD	32(R9), R2
	MOVD	40(R9), R3
	MOVD	0(R9), R10
	BL	(R10)
	MOVD	8(RSP), R30
	ADD	$16, RSP
	RET
