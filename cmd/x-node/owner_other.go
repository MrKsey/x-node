//go:build !unix

package main

// chownToDir is a no-op on platforms without Unix ownership.
func chownToDir(string, string) {}
