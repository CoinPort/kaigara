//go:build cgo

package database

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// SQLiteAvailable reports whether this build can open the in-memory SQLite
// database behind DATABASE_DRIVER=memory. It is false in the released
// binaries, which are built with CGO_ENABLED=0 so they can be static and
// cross-compiled; gorm.io/driver/sqlite needs cgo.
const SQLiteAvailable = true

func sqliteDialector() (gorm.Dialector, error) {
	return sqlite.Open(":memory:"), nil
}
