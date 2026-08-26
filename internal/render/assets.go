package render

import _ "embed"

// These browser assets are embedded into each transcript. Keeping them as
// separate files makes the archived page behavior reviewable and testable.
//
//go:embed transcript.css
var Styles string

//go:embed transcript.js
var Script string
