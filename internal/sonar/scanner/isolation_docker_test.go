//go:build unix && sonardocker

package scanner

// isolatePATH is false under the sonardocker tag, where the whole point is to
// resolve and run the real docker binary.
const isolatePATH = false
