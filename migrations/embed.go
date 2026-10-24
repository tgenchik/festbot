// Package migrations встраивает SQL-миграции в бинарник (goose).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
