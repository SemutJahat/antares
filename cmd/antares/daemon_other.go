//go:build !linux && !darwin && !windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

// Fallback for platforms without a dedicated implementation: background serve is
// not supported, so startDaemon reports it clearly and the user runs
// `antares --foreground`.

func configureDaemonProcess(_ *exec.Cmd) {}
func processAlive(_ int) (bool, error) {
	return false, errors.New("background serve is not supported on this platform; use antares --foreground")
}
func processStartTime(_ int) (string, error) {
	return "", errors.New("background serve is not supported on this platform")
}
func processExecutable(_ int) (string, error) {
	return "", errors.New("background serve is not supported on this platform")
}
func processArguments(_ int) ([]string, error) {
	return nil, errors.New("background serve is not supported on this platform")
}
func processOwnerUID(_ int) (int, error) {
	return 0, errors.New("background serve is not supported on this platform")
}
func processesListeningOnPort(_ int) ([]int, error)         { return nil, nil }
func daemonTargetAlive(pid int, _ bool) (bool, error)       { return processAlive(pid) }
func terminateDaemonProcess(proc *os.Process, _ bool) error { return proc.Signal(os.Interrupt) }
func killDaemonProcess(proc *os.Process, _ bool) error      { return proc.Kill() }
