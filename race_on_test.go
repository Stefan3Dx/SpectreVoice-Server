//go:build race

package main

// raceOn: the tests run under the race detector (ten times slower): the heavy ones do less.
const raceOn = true
