#include "funcdata.h"
#include "textflag.h"

// func cpuHypervisorPresent() bool
//
// CPUID leaf 1 (general information), ECX bit 31 = "Hypervisor present"
// (CPUID.1:ECX.Hypervisor_Present[31]) — set by the hypervisor in a
// VM-guest context. golang.org/x/sys/cpu does not expose this bit, so the
// check lives in this tiny stub (the setup wizard's runtime check
// depends on it).
TEXT ·cpuHypervisorPresent(SB), NOSPLIT, $0-1
	MOVL	$1, AX
	XORL	CX, CX
	CPUID
	TESTL	$0x80000000, CX
	SETNE	AL
	RET
