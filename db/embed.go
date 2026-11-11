// Package db holds GekGo's SQL migrations so any binary can apply them.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
