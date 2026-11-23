//go:build !race

package scheduler

// raceOn: the tests run under the race detector (go test -race).
const raceOn = false
