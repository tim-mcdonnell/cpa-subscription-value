// Package web embeds the single-file dashboard so internal/api can serve it
// from the resource route. go:embed cannot reach outside its package directory,
// which is why the directive lives here rather than in internal/api.
package web

import _ "embed"

// Dashboard is web/dashboard.html verbatim.
//
//go:embed dashboard.html
var Dashboard []byte
