// Package hastebin holds files that are built into the server binary.
package hastebin

import _ "embed"

// AboutMD is the about page. The server stores it as the "about" document
// when no documents are configured.
//
//go:embed about.md
var AboutMD string
