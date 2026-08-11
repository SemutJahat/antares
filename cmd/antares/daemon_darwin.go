//go:build darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// macOS has no /proc, so process identity is read via `ps`. Detachment uses
// setsid so the background server survives the launching shell, matching the
// Linux behaviour (a process group the managed stop path can signal).

func configureDaemonProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processAlive(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	default:
		return false, err
	}
}

// psField runs `ps -o <col>= -p <pid>` and returns the trimmed single value.
func psField(pid int, col string) (string, error) {
	out, err := exec.Command("ps", "-o", col+"=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", errors.New("no such process")
	}
	return v, nil
}

// processStartTime returns a stable per-process token used to detect PID reuse.
// The elapsed-time-since-start (etime) plus the start time (lstart) is stable
// for the life of a process and changes if the PID is recycled.
func processStartTime(pid int) (string, error) {
	return psField(pid, "lstart")
}

func processExecutable(pid int) (string, error) {
	// `ps -o comm=` gives the full executable path on macOS.
	return psField(pid, "comm")
}

func processArguments(pid int) ([]string, error) {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(strings.TrimSpace(string(out))), nil
}

func processOwnerUID(pid int) (int, error) {
	v, err := psField(pid, "uid")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// processesListeningOnPort uses lsof to find the pids listening on a TCP port.
// A missing lsof (rare on macOS) yields no pids rather than an error, so the
// caller falls back to its PID-file state.
func processesListeningOnPort(port int) ([]int, error) {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return nil, nil
	}
	seen := map[int]bool{}
	var pids []int
	for _, line := range strings.Fields(strings.TrimSpace(string(out))) {
		if pid, convErr := strconv.Atoi(line); convErr == nil && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func signalDaemonProcess(proc *os.Process, managed bool, sig syscall.Signal) error {
	if managed {
		return syscall.Kill(-proc.Pid, sig)
	}
	return proc.Signal(sig)
}

func daemonTargetAlive(pid int, managed bool) (bool, error) {
	if !managed {
		return processAlive(pid)
	}
	err := syscall.Kill(-pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func terminateDaemonProcess(proc *os.Process, managed bool) error {
	return signalDaemonProcess(proc, managed, syscall.SIGTERM)
}

func killDaemonProcess(proc *os.Process, managed bool) error {
	return signalDaemonProcess(proc, managed, syscall.SIGKILL)
}
