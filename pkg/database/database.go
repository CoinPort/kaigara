package database

import (
	"fmt"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Internal database pointer
var db *gorm.DB

// Connect to database MySQL/SQLite using gorm
// gorm (GO ORM for SQL): http://gorm.io/docs/connecting_to_the_database.html
// TODO Switch to Config struct
func Connect(cnf *Config) (*gorm.DB, error) {
	var err error
	var dial gorm.Dialector
	var dsn string

	switch cnf.Driver {
	case "memory":
		dial = sqlite.Open(":memory:")

	case "mysql":
		dsn = fmt.Sprintf(
			"%s:%s@tcp(%s:%s)/%s?charset=utf8&parseTime=True&loc=Local",
			cnf.User, cnf.Pass, cnf.Host, cnf.Port, cnf.Name,
		)
		dial = mysql.Open(dsn)

	case "postgres":
		dsn := fmt.Sprintf(
			"user=%s password=%s host=%s port=%s dbname=%s sslmode=disable",
			cnf.User, cnf.Pass, cnf.Host, cnf.Port, cnf.Name,
		)
		dial = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("Unsupported DATABASE_DRIVER: %s", cnf.Driver)
	}

	db, err = gorm.Open(dial, &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// FIXME delete
	sql, err := db.DB()
	if err != nil {
		return nil, err
	}

	// Additional database setup
	switch dsn {
	case "":
		// No setup for sqlite
	default:
		sql.SetMaxOpenConns(cnf.Pool)
	}
	return db, nil
}

// Create the database MySQL/SQLite by name with existing connection
func Create(cnf *Config) error {
	// No need to exec create database cmd for SQlite
	if cnf.Driver == "memory" {
		return nil
	}

	// Connect to the database with given config
	dbName := cnf.Name
	cnf.Name = ""
	db, err := Connect(cnf)
	if err != nil {
		return err
	}
	cnf.Name = dbName

	res := db.Exec(fmt.Sprintf("CREATE DATABASE `%s`;", cnf.Name))
	sql, _ := db.DB()
	sql.Close()
	return res.Error
}

// Drop the database MySQL/SQLite with given db context
func Drop(cnf *Config) error {
	// No need to exec drop database cmd for SQlite
	if cnf.Driver == "memory" {
		// Closing the connection also drops the in-memory database.
		sql, err := db.DB()
		if err != nil {
			return err
		}
		return sql.Close()
	}

	// Connect to the server without selecting the database being dropped.
	dbName := cnf.Name
	cnf.Name = ""
	conn, err := Connect(cnf)
	if err != nil {
		return err
	}
	cnf.Name = dbName

	// Close the connection this function opened, not whichever one the
	// package global happens to point at. `db, err := Connect(cnf)` used to
	// shadow both variables, so the DROP error was discarded and Drop
	// always reported success.
	defer func() {
		if sql, dbErr := conn.DB(); dbErr == nil {
			sql.Close()
		}
	}()

	return conn.Exec(fmt.Sprintf("DROP DATABASE `%s`;", cnf.Name)).Error
}
