package database

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Connect(dsn string, autoMigrate bool, maxOpenConns, maxIdleConns int) (*gorm.DB, error) {
	if err := ensureDatabase(dsn); err != nil {
		return nil, err
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// Defaults (database/sql's MaxIdleConns is 2, MaxOpenConns unbounded)
	// cause connection churn and can pile up past Postgres's own
	// max_connections under real concurrency, well before the host itself
	// is short on CPU.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)

	if autoMigrate {
		if err := Migrate(db); err != nil {
			return nil, err
		}
	}

	return db, nil
}

// Migrate creates or updates the tables of every model.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&models.User{}, &models.Consumer{}, &models.ProviderKey{}, &models.UserProviderAccess{}, &models.Usage{}, &models.APIKey{})
}

func ensureDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return err
	}

	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" {
		return fmt.Errorf("database name missing from DSN")
	}

	maintenanceURL := *u
	maintenanceURL.Path = "/postgres"

	conn, err := sql.Open("pgx", maintenanceURL.String())
	if err != nil {
		return err
	}
	defer conn.Close()

	var exists bool
	err = conn.QueryRow("SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", dbName).Scan(&exists)
	if err != nil {
		return err
	}

	if !exists {
		_, err = conn.Exec(fmt.Sprintf("CREATE DATABASE %s", pgx.Identifier{dbName}.Sanitize()))
		if err != nil && !createdMeanwhile(err) {
			return err
		}
	}

	return nil
}

// createdMeanwhile reports whether CREATE DATABASE failed only because another
// instance, starting at the same time, created the database first. Postgres
// reports that as duplicate_database or, when both try at the same moment, as
// a unique violation on the database name.
func createdMeanwhile(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && (pgErr.Code == "42P04" || pgErr.Code == "23505")
}

func IsUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}

	return pgErr.Code == "23505"
}
