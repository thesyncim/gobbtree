#include "textflag.h"

// func noescape(p unsafe.Pointer) unsafe.Pointer
TEXT ·noescape(SB), NOSPLIT, $0-16
	MOVQ	p+0(FP), AX
	MOVQ	AX, ret+8(FP)
	RET
