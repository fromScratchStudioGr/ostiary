package auth

import "github.com/golang-jwt/jwt/v5"

type LoginRequest struct {
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type LoginResponse struct {
	Username string `json:"username"`
}

type Claims struct {
	UserID uint `json:"user_id"`
	jwt.RegisteredClaims
}
