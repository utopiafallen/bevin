#include "funcdata.h"
#include "textflag.h"

// func cpuHypervisorPresent() bool
//
// CPUID leaf 1 (general information), ECX bit 31 = "Hypervisor present"
// (CPUID.1:ECX.Hypervisor_Present[31]). The hypervisor sets this bit in
// EVERY partition it runs — including the elevated root partition — so on
// a bare-metal Windows host with the Hyper-V Platform feature enabled it
// is set in host processes too (this is the same bit behind systeminfo's
// "A hypervisor has been detected" line). golang.org/x/sys/cpu does not
// expose it (it enumerates CPU capabilities, not the virtualization
// context), so the check lives in this tiny stub (the setup wizard's
// runtime check depends on it).
//
// ABI note: under Go's amd64 ABI0 the caller reads the return value from
// ret+0(FP) on the stack — the result in AL after SETNE must be STORED
// there before RET (leaving it in AX and RETing returns a garbage stack
// byte — see the vet "RET without writing to 1-byte ret+0(FP)" error this
// used to trip).
TEXT ·cpuHypervisorPresent(SB), NOSPLIT, $0-1
	MOVL	$1, AX
	XORL	CX, CX
	CPUID
	TESTL	$0x80000000, CX
	SETNE	AL
	MOVB	AL, ret+0(FP)
	RET
