// This is an external test package because testutil imports database, which
// would make an in-package test import itself.
package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/database"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// serverURL returns the test Postgres' URL, as the superuser it's configured
// with.
func serverURL(t *testing.T) url.URL {
	t.Helper()
	u, err := url.Parse(testutil.PostgresDSN(t))
	if err != nil {
		t.Fatalf("parsing %s: %v", testutil.PostgresEnv, err)
	}
	return *u
}

// adminExec runs a statement on the server through its maintenance database,
// for the statements that can't run inside the database a test is using.
func adminExec(server url.URL, statement string) error {
	server.Path = "/postgres"
	admin, err := sql.Open("pgx", server.String())
	if err != nil {
		return err
	}
	defer admin.Close()
	_, err = admin.Exec(statement)
	return err
}

// freshDatabase returns the DSN of a database that doesn't exist yet on the
// test Postgres, and its name. The database is dropped when the test ends.
func freshDatabase(t *testing.T) (dsn, name string) {
	t.Helper()
	server := serverURL(t)
	name = "lg_test_" + testutil.Unique()[:12]

	t.Cleanup(func() {
		// FORCE disconnects whatever the test left open.
		if err := adminExec(server, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})

	fresh := server
	fresh.Path = "/" + name
	return fresh.String(), name
}

// connect is database.Connect that fails the test on error and closes the
// connection when the test ends.
func connect(t *testing.T, dsn string, autoMigrate bool, maxOpen, maxIdle int) *gorm.DB {
	t.Helper()
	db, err := database.Connect(dsn, autoMigrate, maxOpen, maxIdle)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return db
}

var allModels = []any{
	&models.User{},
	&models.Consumer{},
	&models.ProviderKey{},
	&models.UserProviderAccess{},
	&models.Usage{},
	&models.APIKey{},
}

func TestConnect(t *testing.T) {
	t.Run("rejects a DSN it can't use, and returns no db", func(t *testing.T) {
		tests := []struct {
			name    string
			dsn     string
			wantErr string
		}{
			{"unparseable", "://bad", "missing protocol scheme"},
			{"no database name", "postgres://postgres:test@127.0.0.1:1", "database name missing"},
			{"empty database name", "postgres://postgres:test@127.0.0.1:1/", "database name missing"},
			{"a setting the driver rejects", "postgres://postgres:test@127.0.0.1:1/ostiary?sslmode=bogus", "sslmode"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				db, err := database.Connect(tt.dsn, true, 1, 1)

				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
				}
				if db != nil {
					t.Error("returned a db alongside the error")
				}
			})
		}
	})

	t.Run("returns an error, and no db, when the server can't be reached", func(t *testing.T) {
		db, err := database.Connect("postgres://postgres:test@127.0.0.1:1/ostiary?sslmode=disable&connect_timeout=2", true, 1, 1)

		if err == nil {
			t.Error("got nil error, want one")
		}
		if db != nil {
			t.Error("returned a db alongside the error")
		}
	})

	t.Run("returns an error, and no db, when the database exists but can't be connected to", func(t *testing.T) {
		dsn, name := freshDatabase(t)
		server := serverURL(t)
		if err := adminExec(server, "CREATE DATABASE "+name); err != nil {
			t.Fatalf("creating the database: %v", err)
		}
		if err := adminExec(server, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS false"); err != nil {
			t.Fatalf("closing the database to connections: %v", err)
		}

		db, err := database.Connect(dsn, true, 1, 1)

		if err == nil || !strings.Contains(err.Error(), "not currently accepting connections") {
			t.Errorf("error = %v, want one saying the database isn't accepting connections", err)
		}
		if db != nil {
			t.Error("returned a db alongside the error")
		}
	})

	t.Run("returns an error, and no db, when the database can't be created", func(t *testing.T) {
		// A role that may log in but not create databases.
		role := "lg_role_" + testutil.Unique()[:12]
		server := serverURL(t)
		if err := adminExec(server, "CREATE ROLE "+role+" LOGIN PASSWORD 'pw'"); err != nil {
			t.Fatalf("creating the role: %v", err)
		}
		t.Cleanup(func() {
			if err := adminExec(server, "DROP ROLE IF EXISTS "+role); err != nil {
				t.Errorf("dropping %s: %v", role, err)
			}
		})
		dsn, _ := freshDatabase(t)
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, "pw")

		db, err := database.Connect(u.String(), true, 1, 1)

		if err == nil || !strings.Contains(err.Error(), "permission denied to create database") {
			t.Errorf("error = %v, want one saying the role can't create the database", err)
		}
		if db != nil {
			t.Error("returned a db alongside the error")
		}
	})

	t.Run("returns an error, and no db, when the tables can't be created", func(t *testing.T) {
		dsn, _ := freshDatabase(t)
		// Something that isn't a table is already called "users".
		first := connect(t, dsn, false, 5, 5)
		if err := first.Exec("CREATE VIEW users AS SELECT 1 AS id").Error; err != nil {
			t.Fatalf("creating the view: %v", err)
		}

		db, err := database.Connect(dsn, true, 1, 1)

		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("error = %v, want one saying users already exists", err)
		}
		if db != nil {
			t.Error("returned a db alongside the error")
		}
	})

	t.Run("creates the database named in the DSN when it doesn't exist", func(t *testing.T) {
		dsn, name := freshDatabase(t)

		db := connect(t, dsn, false, 5, 5)

		var current string
		if err := db.Raw("SELECT current_database()").Scan(&current).Error; err != nil {
			t.Fatalf("querying: %v", err)
		}
		if current != name {
			t.Errorf("connected to %q, want %q", current, name)
		}
	})

	t.Run("lets several instances start at once against a database that doesn't exist yet", func(t *testing.T) {
		dsn, _ := freshDatabase(t)
		start := make(chan struct{})
		errs := make([]error, 8)

		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				<-start
				db, err := database.Connect(dsn, false, 1, 1)
				if err == nil {
					sqlDB, _ := db.DB()
					sqlDB.Close()
				}
				errs[i] = err
			})
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("instance %d: %v", i, err)
			}
		}
	})

	t.Run("connects to an existing database, leaving its contents alone", func(t *testing.T) {
		dsn, _ := freshDatabase(t)
		first := connect(t, dsn, false, 5, 5)
		if err := first.Exec("CREATE TABLE marker (id int)").Error; err != nil {
			t.Fatalf("creating marker table: %v", err)
		}

		second := connect(t, dsn, false, 5, 5)

		if !second.Migrator().HasTable("marker") {
			t.Error("the second Connect didn't see the first one's table")
		}
	})

	t.Run("limits open and idle connections as asked", func(t *testing.T) {
		dsn, _ := freshDatabase(t)
		db := connect(t, dsn, false, 3, 1)
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}

		if got := sqlDB.Stats().MaxOpenConnections; got != 3 {
			t.Errorf("max open connections = %d, want 3", got)
		}

		// Use all three, then give them back: only one may stay pooled.
		ctx := context.Background()
		var conns []*sql.Conn
		for range 3 {
			conn, err := sqlDB.Conn(ctx)
			if err != nil {
				t.Fatalf("taking a connection: %v", err)
			}
			conns = append(conns, conn)
		}
		for _, conn := range conns {
			conn.Close()
		}
		if got := sqlDB.Stats().Idle; got != 1 {
			t.Errorf("idle connections = %d, want 1", got)
		}
	})

	t.Run("creates the tables only when asked to migrate", func(t *testing.T) {
		for _, migrate := range []bool{false, true} {
			t.Run(fmt.Sprintf("autoMigrate=%v", migrate), func(t *testing.T) {
				dsn, _ := freshDatabase(t)

				db := connect(t, dsn, migrate, 5, 5)

				if got := db.Migrator().HasTable(&models.User{}); got != migrate {
					t.Errorf("users table exists = %v, want %v", got, migrate)
				}
			})
		}
	})
}

func TestMigrate(t *testing.T) {
	dsn, _ := freshDatabase(t)
	db := connect(t, dsn, false, 5, 5)

	if err := database.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	t.Run("creates the table of every model", func(t *testing.T) {
		for _, model := range allModels {
			if !db.Migrator().HasTable(model) {
				t.Errorf("no table for %T", model)
			}
		}
	})

	t.Run("can run again on a migrated database", func(t *testing.T) {
		if err := database.Migrate(db); err != nil {
			t.Errorf("second Migrate: %v", err)
		}
	})
}

func TestIsUniqueViolation(t *testing.T) {
	pgErr := func(code string) error { return &pgconn.PgError{Code: code} }

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"an error that isn't from Postgres", errors.New("duplicate key"), false},
		{"a unique violation", pgErr("23505"), true},
		{"a unique violation that was wrapped", fmt.Errorf("creating user: %w", pgErr("23505")), true},
		{"another constraint violation", pgErr("23503"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := database.IsUniqueViolation(tt.err); got != tt.want {
				t.Errorf("IsUniqueViolation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}

	// The cases above build the error by hand. These check that what gorm
	// really returns from Postgres is recognised the same way.
	t.Run("recognises the error gorm returns for a duplicate unique value", func(t *testing.T) {
		db := testutil.Postgres(t)
		if err := db.Create(&models.User{Username: "alice"}).Error; err != nil {
			t.Fatalf("creating the first user: %v", err)
		}

		err := db.Create(&models.User{Username: "alice"}).Error

		if !database.IsUniqueViolation(err) {
			t.Errorf("IsUniqueViolation(%v) = false, want true", err)
		}
	})

	t.Run("doesn't mistake another Postgres error for one", func(t *testing.T) {
		db := testutil.Postgres(t)

		err := db.Exec("SELECT * FROM no_such_table").Error

		if err == nil {
			t.Fatal("the query succeeded")
		}
		if database.IsUniqueViolation(err) {
			t.Errorf("IsUniqueViolation(%v) = true, want false", err)
		}
	})
}
