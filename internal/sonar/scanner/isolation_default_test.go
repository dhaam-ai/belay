//go:build unix && !sonardocker

package scanner

// isolatePATH empties PATH for the test binary in the default build.
//
// Every test in the default build drives the Scanner through a stub Execer and
// must never start a process. An empty PATH means that if one ever slipped
// through to a real *exec.Runner, the lookup for "docker" would fail outright
// instead of quietly contacting a daemon — which turns a silent violation of
// ADR 0008's "the suite runs without SonarQube or Docker" promise into a loud
// failure.
const isolatePATH = true
