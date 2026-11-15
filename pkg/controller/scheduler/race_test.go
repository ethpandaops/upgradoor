//go:build race

package scheduler

// raceDetector says whether the tests run under the race detector, which
// makes everything several times slower and timings meaningless.
const raceDetector = true
