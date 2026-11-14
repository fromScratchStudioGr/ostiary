package main

import (
	"log/slog"

	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// seedDefaultAdmin creates a default "admin"/"admin" account the first time
// the admin panel runs against an empty users table, so there's always a
// way in. Change its password (or create another admin and revoke this one)
// right after first login.
func seedDefaultAdmin(db *gorm.DB) error {
	var count int64
	if err := db.Model(&models.User{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)

	if err := db.Create(&models.User{Username: "admin", Password: string(hashedPassword)}).Error; err != nil {
		return err
	}

	slog.Warn("seeded default admin account (username: admin, password: admin) — change its password")
	return nil
}
