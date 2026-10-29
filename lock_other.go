//go:build !unix

package main

// lockData: no file locks here (the server is built for Linux); restore can't tell whether the server runs.
func lockData(string) (func(), error) { return func() {}, nil }

func freeBytes(string) (int64, bool) { return 0, false }
