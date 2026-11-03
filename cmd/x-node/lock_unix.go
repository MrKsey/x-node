//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockFile takes an exclusive advisory lock shared by the service and the CLI.
// The lock is released when the returned function is called.
func lockFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// A root CLI run must not leave a root-only lock file behind: the service
	// runs as an unprivileged user and would not be able to open it.
	chownToDir(path, filepath.Dir(path))
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
