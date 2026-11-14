package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/fromScratchStudioGr/ostiary/internal/api"
	"github.com/fromScratchStudioGr/ostiary/internal/config"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"github.com/go-playground/validator/v10"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var validate = validator.New()

func LoginHandler(cfg config.Config, db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			slog.Warn("login: invalid payload", "error", err)
			api.WriteError(w, "Invalid payload", http.StatusBadRequest)
			return
		}

		if err := validate.Struct(req); err != nil {
			slog.Warn("login: validation failed", "username", req.Username, "error", err)
			api.WriteError(w, err.Error(), http.StatusBadRequest)
			return
		}

		var user models.User
		if err := db.WithContext(r.Context()).Where("username = ?", req.Username).First(&user).Error; err != nil {
			slog.Warn("login: user not found", "username", req.Username, "error", err)
			api.WriteError(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
			slog.Warn("login: invalid password", "username", req.Username, "error", err)
			api.WriteError(w, "Invalid credentials", http.StatusUnauthorized)
			return
		}

		if user.Status != "active" {
			slog.Warn("login: user is revoked", "username", req.Username)
			api.WriteError(w, "Account access revoked", http.StatusForbidden)
			return
		}

		claims := Claims{
			UserID: user.ID,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   user.Username,
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(cfg.JWTAuthentication.ExpirationInHours) * time.Hour)),
			},
		}

		signedToken, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.JWTAuthentication.Secret))

		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    signedToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   cookieSecure(cfg),
			SameSite: cfg.CookieSameSite,
			MaxAge:   cfg.JWTAuthentication.ExpirationInHours * 60 * 60,
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(
			LoginResponse{
				Username: user.Username,
			},
		)
	}
}
