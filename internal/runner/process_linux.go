package runner

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func processAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func processIdentity(pid int) (string, string, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", "", err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return "", "", fmt.Errorf("cannot identify the spawned process")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return "", "", fmt.Errorf("cannot identify the spawned process")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(boot)), fields[19], err
}
