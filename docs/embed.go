// Package docs embeds the customer cutover guide in prepared output.
package docs

import _ "embed"

// Cutover is the operator procedure included in every prepared split.
//
//go:embed cutover.md
var Cutover string
