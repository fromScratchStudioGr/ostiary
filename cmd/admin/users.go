package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/auth"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// adminOnly lets a request through only for a logged-in admin whose account is
// still active.
func adminOnly(cfg config.Config, db *gorm.DB, next http.HandlerFunc) http.HandlerFunc {
	return auth.RequireJWT(cfg, requireActiveUser(db, next))
}

func requireActiveUser(db *gorm.DB, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r)
		if claims == nil {
			slog.Warn("requireActiveUser: no claims in context", "path", r.URL.Path)
			api.WriteError(w, "Forbidden", http.StatusForbidden)
			return
		}

		var user models.User
		if err := db.WithContext(r.Context()).First(&user, claims.UserID).Error; err != nil {
			slog.Warn("requireActiveUser: user not found", "user_id", claims.UserID, "error", err)
			api.WriteError(w, "Forbidden", http.StatusForbidden)
			return
		}

		if user.Status != "active" {
			slog.Warn("requireActiveUser: user is revoked", "user_id", claims.UserID)
			api.WriteError(w, "User access revoked", http.StatusForbidden)
			return
		}

		next(w, r)
	}
}

// passwordProblem returns why a new admin password is too weak, or "" if it
// is strong enough. It is the same rule the panel checks.
func passwordProblem(password string) string {
	var upper, lower, digit, symbol bool
	for _, c := range password {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		default:
			symbol = true
		}
	}

	switch {
	case utf8.RuneCountInString(password) < 8:
		return "Password must be at least 8 characters"
	case !upper:
		return "Password must include an uppercase letter"
	case !lower:
		return "Password must include a lowercase letter"
	case !digit:
		return "Password must include a number"
	case !symbol:
		return "Password must include a symbol"
	}
	return ""
}

func meHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		claims := auth.ClaimsFromContext(r)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.MeResponse{Username: claims.Subject})
	})
}

func registerHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("register: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("register: validation failed", "username", req.Username, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		if req.Password != req.PasswordConfirm {
			slog.Warn("register: passwords do not match", "username", req.Username)
			api.WriteError(w, "Passwords do not match", http.StatusBadRequest)
			return
		}

		if problem := passwordProblem(req.Password); problem != "" {
			slog.Warn("register: password too weak", "username", req.Username)
			api.WriteError(w, problem, http.StatusBadRequest)
			return
		}

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			slog.Error("register: could not hash password", "username", req.Username, "error", err)
			api.WriteError(w, "Could not hash password", http.StatusInternalServerError)
			return
		}

		user := models.User{
			Username: req.Username,
			Password: string(hashedPassword),
		}

		if err := db.WithContext(r.Context()).Create(&user).Error; err != nil {
			if database.IsUniqueViolation(err) {
				slog.Warn("register: account already exists", "username", req.Username, "error", err)
				api.WriteError(w, "Could not create account", http.StatusConflict)
				return
			}
			slog.Error("register: could not create account", "username", req.Username, "error", err)
			api.WriteError(w, "Could not create account", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "Account created for %s", user.Username)
	})
}

func getUsersHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var users []models.User
		if err := db.WithContext(r.Context()).Find(&users).Error; err != nil {
			slog.Error("get-users: could not fetch users", "error", err)
			api.WriteError(w, "Could not fetch users", http.StatusInternalServerError)
			return
		}

		response := make([]api.UserResponse, len(users))
		for i, u := range users {
			response[i] = api.UserResponse{
				ID:        u.ID,
				Username:  u.Username,
				Status:    u.Status,
				CreatedAt: u.CreatedAt,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
}

// updateUserPasswordHandler lets an admin set another admin's password
// (or their own). Admin-only.
func updateUserPasswordHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateUserPasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("update-user-password: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("update-user-password: validation failed", "user_id", req.UserId, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		if req.Password != req.PasswordConfirm {
			slog.Warn("update-user-password: passwords do not match", "user_id", req.UserId)
			api.WriteError(w, "Passwords do not match", http.StatusBadRequest)
			return
		}

		if problem := passwordProblem(req.Password); problem != "" {
			slog.Warn("update-user-password: password too weak", "user_id", req.UserId)
			api.WriteError(w, problem, http.StatusBadRequest)
			return
		}

		var user models.User
		if err := db.WithContext(r.Context()).First(&user, req.UserId).Error; err != nil {
			slog.Warn("update-user-password: user not found", "user_id", req.UserId, "error", err)
			api.WriteError(w, "User not found", http.StatusNotFound)
			return
		}

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			slog.Error("update-user-password: could not hash password", "user_id", req.UserId, "error", err)
			api.WriteError(w, "Could not hash password", http.StatusInternalServerError)
			return
		}

		if err := db.WithContext(r.Context()).Model(&user).Update("password", string(hashedPassword)).Error; err != nil {
			slog.Error("update-user-password: could not update password", "user_id", req.UserId, "error", err)
			api.WriteError(w, "Could not update password", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}

// deleteUserHandler deletes an admin account by id. Admin-only. An admin
// cannot delete themselves — since the caller is necessarily a row in the
// same users table (requireActiveUser), this also transitively blocks
// deleting the last remaining admin: if only one admin exists, it can only
// be the caller.
func deleteUserHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return adminOnly(cfg, db, func(w http.ResponseWriter, r *http.Request) {
		userID, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			slog.Warn("delete-user: invalid user id", "id", r.PathValue("id"), "error", err)
			api.WriteError(w, "Invalid user id", http.StatusBadRequest)
			return
		}

		claims := auth.ClaimsFromContext(r)
		if claims != nil && uint64(claims.UserID) == userID {
			slog.Warn("delete-user: admin cannot delete themselves", "user_id", userID)
			api.WriteError(w, "Cannot delete your own account", http.StatusForbidden)
			return
		}

		var user models.User
		if err := db.WithContext(r.Context()).First(&user, userID).Error; err != nil {
			slog.Warn("delete-user: user not found", "user_id", userID, "error", err)
			api.WriteError(w, "User not found", http.StatusNotFound)
			return
		}

		if err := db.WithContext(r.Context()).Delete(&user).Error; err != nil {
			slog.Error("delete-user: could not delete user", "user_id", userID, "error", err)
			api.WriteError(w, "Could not delete user", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}
