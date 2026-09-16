//go:build !windows

package gatecontext

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func processParentPID(pid int) (int, error) {
	if pid <= 1 {
		return 0, nil
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		// The authenticated client can disappear only after sending its request;
		// treat an already-gone process as the end of the chain.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return 0, nil
		}
		return 0, err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return 0, nil
	}
	ppid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse parent pid: %w", err)
	}
	return ppid, nil
}
