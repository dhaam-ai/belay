//go:build tools

// Package deps tracks tool dependencies.
// This file uses blank imports to ensure that dependencies required for builds
// and code generation are not removed by `go mod tidy`.
package deps

import (
	_ "github.com/google/go-cmp/cmp"
	_ "github.com/spf13/cobra"
	_ "gopkg.in/yaml.v3"
)
