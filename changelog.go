// Package dlmanager holds the project's changelog (CHANGELOG.md), built into
// the app so it can show what's new after an update, even offline.
package dlmanager

import _ "embed"

//go:embed CHANGELOG.md
var Changelog string
