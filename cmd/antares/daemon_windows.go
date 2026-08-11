//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Windows has no fork/setsid. A detached background process is created with
// CREATE_NEW_PROCESS_GROUP | DETACHED_PROCESS so it outlives the launching
// console. Process identity is read via `tasklist`, port owners via `netstat`.

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func configureDaemonProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}

func processAlive(pid int) (bool, error) {
	line, err := tasklistLine(pid)
	if err != nil {
		return false, nil
	}
	return line != "", nil
}

// tasklistLine returns the CSV row for a pid, or "" when it is not running.
func tasklistLine(pid int) (string, error) {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	// When nothing matches, tasklist prints an INFO line rather than a CSV row.
	if s == "" || strings.HasPrefix(s, "INFO:") {
		return "", nil
	}
	return s, nil
}

func processStartTime(pid int) (string, error) {
	// tasklist does not expose start time cheaply; the image name is a stable
	// enough identity token for PID-reuse detection alongside the alive check.
	line, err := tasklistLine(pid)
	if err != nil || line == "" {
		return "", errors.New("no such process")
	}
	if fields := strings.SplitN(strings.Trim(line, "\""), "\",\"", 2); len(fields) > 0 {
		return fields[0], nil
	}
	return line, nil
}

func processExecutable(pid int) (string, error) {
	line, err := tasklistLine(pid)
	if err != nil || line == "" {
		return "", errors.New("no such process")
	}
	fields := strings.SplitN(strings.Trim(line, "\""), "\",\"", 2)
	if len(fields) > 0 {
		return fields[0], nil // image name
	}
	return "", errors.New("could not read process image")
}

func processArguments(_ int) ([]string, error) {
	// Command-line arguments are not readily available via tasklist; identity is
	// established via the image name and the PID-file state instead.
	return nil, nil
}

func processOwnerUID(_ int) (int, error) {
	// Windows has no numeric UID; ownership is not enforced here.
	return 0, nil
}

func processesListeningOnPort(port int) ([]int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "TCP").Output()
	if err != nil {
		return nil, nil
	}
	want := ":" + strconv.Itoa(port)
	seen := map[int]bool{}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[3], "LISTENING") {
			continue
		}
		if !strings.HasSuffix(fields[1], want) {
			continue
		}
		if pid, convErr := strconv.Atoi(fields[4]); convErr == nil && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func daemonTargetAlive(pid int, _ bool) (bool, error) { return processAlive(pid) }

func terminateDaemonProcess(proc *os.Process, _ bool) error {
	// No POSIX signals; taskkill terminates the process tree.
	return exec.Command("taskkill", "/PID", strconv.Itoa(proc.Pid), "/T").Run()
}

func killDaemonProcess(proc *os.Process, _ bool) error {
	return exec.Command("taskkill", "/PID", strconv.Itoa(proc.Pid), "/T", "/F").Run()
}
