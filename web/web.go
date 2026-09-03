// Package web carries the governance dashboard as a compiled-in asset.
//
// The file lives here, beside the other non-Go assets, rather than inside
// internal/api, because that is where a person looks for a page. It is a Go
// package rather than a bare file because go:embed cannot reach outside its own
// directory: something has to declare the embed, and a five-line package next
// to the asset is a smaller compromise than moving the asset next to the code
// that serves it.
//
// The consequence worth stating is that the binary is self-contained. There is
// no asset directory to deploy, no path to configure, and no way for the page a
// server hands out to disagree with the API it was built against.
package web

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Dashboard returns the governance dashboard's HTML.
//
// It returns a copy, so a caller that writes into the slice cannot corrupt the
// page every later request is served from.
func Dashboard() []byte {
	out := make([]byte, len(dashboardHTML))
	copy(out, dashboardHTML)
	return out
}

// DashboardETag is the content hash of the page, used as a strong HTTP entity
// tag so a browser polling the API does not refetch the shell on every load.
var DashboardETag = func() string {
	sum := sha256.Sum256(dashboardHTML)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}()
