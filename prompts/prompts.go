// Package prompts embeds the system prompts for each LLM stage. They are
// frozen per stage so the API can cache them across requests.
package prompts

import _ "embed"

//go:embed extract.md
var Extract string

//go:embed merge.md
var Merge string

//go:embed score.md
var Score string
