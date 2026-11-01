package usagedashboard

import _ "embed"

// Page is served locally; all usage data stays behind management authentication.
//
//go:embed usage.html
var Page []byte
