//go:build !cgo

package database

import (
	"errors"

	"gorm.io/gorm"
)

// SQLiteAvailable is false here: gorm.io/driver/sqlite is not compiled into
// this build, so github.com/mattn/go-sqlite3 is not linked either. Without
// this split it was linked but inert -- every call returned "Binary was
// compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work. This is a
// stub", which named a library the operator never configured.
const SQLiteAvailable = false

// ErrSQLiteUnavailable is returned for DATABASE_DRIVER=memory in a build
// without cgo.
var ErrSQLiteUnavailable = errors.New(
	"DATABASE_DRIVER=memory needs SQLite, which requires cgo, and this binary was built with CGO_ENABLED=0. " +
		"Released Kaigara binaries are all built that way so they stay static. " +
		"The memory driver exists for the test suite and does not persist anything; " +
		"use DATABASE_DRIVER=mysql or postgres instead")

func sqliteDialector() (gorm.Dialector, error) {
	return nil, ErrSQLiteUnavailable
}
