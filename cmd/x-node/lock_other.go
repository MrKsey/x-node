//go:build !unix

package main

// lockFile is a no-op on platforms without flock. The daemon targets Linux;
// this stub only keeps the package testable on a developer machine.
func lockFile(string) (func(), error) {
	return func() {}, nil
}
