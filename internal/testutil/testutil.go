// Package testutil gives tests the real Postgres and Redis they run against,
// each isolated from other tests. It's only imported from _test.go files.
//
// Both services are external: tests find them through TEST_POSTGRES_ADDR (a
// Postgres DSN) and TEST_REDIS_ADDR (host:port). When those aren't set the
// test is skipped, so a plain `go test ./...` works on a machine without
// them, but when CI is set (GitHub Actions does) it fails instead, so a
// missing service can never turn into silently skipped tests. The tests
// service in docker-compose.test.yml starts both services and sets the
// variables; see the README.
package testutil

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/database"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	PostgresEnv = "TEST_POSTGRES_ADDR"
	RedisEnv    = "TEST_REDIS_ADDR"
)

// Unique returns a random identifier, for keys and names that must not
// collide with another test's.
func Unique() string {
	return strings.ToLower(rand.Text())
}

// PostgresDSN returns TEST_POSTGRES_ADDR, for tests that need the server
// itself rather than a schema in it, such as ones that create databases.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	return requireEnv(t, PostgresEnv)
}

// Postgres returns a connection to a fresh, empty schema of the Postgres at
// TEST_POSTGRES_ADDR, with every model migrated. The schema is dropped when
// the test ends, so tests never see each other's rows.
func Postgres(t testing.TB) *gorm.DB {
	t.Helper()
	dsn := requireEnv(t, PostgresEnv)

	// database.Connect also creates the database named in the DSN if the
	// server doesn't have it yet, as on a fresh service container.
	admin, err := database.Connect(dsn, false, 2, 2)
	if err != nil {
		t.Fatalf("connecting to Postgres: %v", err)
	}

	schema := "t_" + Unique()[:12]
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		closeDB(admin)
		t.Fatalf("creating test schema: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		closeDB(admin)
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing %s: %v", PostgresEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := database.Connect(u.String(), false, 5, 5)
	if err != nil {
		t.Fatalf("connecting to the test schema: %v", err)
	}
	t.Cleanup(func() { closeDB(db) })

	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating the test schema: %v", err)
	}
	return db
}

// Redis returns the address of the Redis at TEST_REDIS_ADDR and a client for
// it. Unlike Postgres there's no per-test isolation: tests are expected to
// use keys made unique with Unique, and to remove the ones they create.
func Redis(t testing.TB) (string, *redis.Client) {
	t.Helper()
	addr := requireEnv(t, RedisEnv)

	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("connecting to Redis: %v", err)
	}
	return addr, client
}

func requireEnv(t testing.TB, name string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping in -short mode: needs a real Postgres/Redis")
	}
	if v := os.Getenv(name); v != "" {
		return v
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s is not set, but tests that need it must not be skipped in CI", name)
	}
	t.Skipf("%s not set; run these tests with: docker compose -f docker-compose.test.yml run --rm tests", name)
	return ""
}

func closeDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.Close()
	}
}
