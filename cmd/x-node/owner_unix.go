//go:build unix

package main

import (
	"os"
	"syscall"
)

// chownToDir gives a file the owner and group of the state directory. The CLI
// may run as root while the service runs as an unprivileged user; without this
// the service could not read an invite created by the CLI.
func chownToDir(path, dir string) {
	if os.Geteuid() != 0 {
		return
	}
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	_ = os.Chown(path, int(stat.Uid), int(stat.Gid))
}
