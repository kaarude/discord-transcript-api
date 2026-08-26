package web

import _ "embed"

//go:embed favicon.svg
var Favicon string

//go:embed access.html
var AccessHTML string

//go:embed admin.html
var AdminHTML string

//go:embed health.html
var HealthHTML string
