package main

import (
	"testing"

	"github.com/fromScratchStudioGr/ostiary/internal/database/models"
	"github.com/fromScratchStudioGr/ostiary/internal/testutil"

	"golang.org/x/crypto/bcrypt"
)

func TestSeedDefaultAdmin(t *testing.T) {
	t.Run("seeds an active admin/admin account into an empty users table", func(t *testing.T) {
		db := testutil.Postgres(t)

		if err := seedDefaultAdmin(db); err != nil {
			t.Fatalf("seedDefaultAdmin: %v", err)
		}

		var users []models.User
		if err := db.Find(&users).Error; err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 {
			t.Fatalf("got %d users, want 1", len(users))
		}
		if users[0].Username != "admin" {
			t.Errorf("username = %q, want admin", users[0].Username)
		}
		if users[0].Status != "active" {
			t.Errorf("status = %q, want active", users[0].Status)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(users[0].Password), []byte("admin")); err != nil {
			t.Errorf("stored password is not a bcrypt hash of \"admin\": %v", err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		db := testutil.Postgres(t)

		for range 2 {
			if err := seedDefaultAdmin(db); err != nil {
				t.Fatalf("seedDefaultAdmin: %v", err)
			}
		}

		var count int64
		db.Model(&models.User{}).Count(&count)
		if count != 1 {
			t.Errorf("got %d users after seeding twice, want 1", count)
		}
	})

	t.Run("leaves an existing user table untouched", func(t *testing.T) {
		db := testutil.Postgres(t)
		existing := models.User{Username: "alice", Password: "hash"}
		if err := db.Create(&existing).Error; err != nil {
			t.Fatal(err)
		}

		if err := seedDefaultAdmin(db); err != nil {
			t.Fatalf("seedDefaultAdmin: %v", err)
		}

		var users []models.User
		db.Find(&users)
		if len(users) != 1 || users[0].Username != "alice" || users[0].Password != "hash" {
			t.Errorf("users = %+v, want only the original alice", users)
		}
	})

	t.Run("reports a failure to create when a soft-deleted admin still holds the username", func(t *testing.T) {
		db := testutil.Postgres(t)
		old := models.User{Username: "admin", Password: "hash"}
		if err := db.Create(&old).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Delete(&old).Error; err != nil {
			t.Fatal(err)
		}

		if err := seedDefaultAdmin(db); err == nil {
			t.Fatal("expected an error because the unique username is taken by the deleted row")
		}
	})

	t.Run("reports a failure to count users", func(t *testing.T) {
		db := testutil.Postgres(t)
		if err := db.Migrator().DropTable(&models.User{}); err != nil {
			t.Fatal(err)
		}

		if err := seedDefaultAdmin(db); err == nil {
			t.Fatal("expected an error when the users table is missing")
		}
	})
}
