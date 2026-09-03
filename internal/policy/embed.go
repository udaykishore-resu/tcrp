package policy

import (
	_ "embed"
)

// The default bundle is embedded so the platform runs from a clean clone with
// no configuration. Operators override it with --policy, and CI compiles both
// the embedded bundle and any override before either is installed.
//
//go:embed bundles/collateral-ops.json
var defaultBundleJSON []byte

// DefaultBundleJSON returns the embedded bundle source.
func DefaultBundleJSON() []byte { return defaultBundleJSON }

// Default returns the compiled default bundle.
func Default() (*Bundle, error) { return LoadBundle(defaultBundleJSON) }

// MustDefault returns the compiled default bundle or panics. A binary that
// cannot compile its own embedded policy must not start.
func MustDefault() *Bundle {
	b, err := Default()
	if err != nil {
		panic(err)
	}
	return b
}
