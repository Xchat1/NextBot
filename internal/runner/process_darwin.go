package runner

/*
#include <libproc.h>
#include <sys/proc_info.h>
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

func processAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func processIdentity(pid int) (string, string, error) {
	var info C.struct_proc_bsdinfo
	size := C.int(unsafe.Sizeof(info))
	if n, err := C.proc_pidinfo(C.int(pid), C.PROC_PIDTBSDINFO, 0, unsafe.Pointer(&info), size); n != size {
		return "", "", fmt.Errorf("cannot identify process %d: %v", pid, err)
	}
	boot, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", "", err
	}
	return boot, fmt.Sprintf("%d.%06d", info.pbi_start_tvsec, info.pbi_start_tvusec), nil
}
