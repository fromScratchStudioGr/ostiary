package models

import (
	"time"

	"gorm.io/gorm"
)

type ProviderKey struct {
	ID              uint `gorm:"primaryKey"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       gorm.DeletedAt `gorm:"index"`
	Provider        string         `gorm:"index;size:32"` // 'anthropic', 'deepseek', 'openai'
	Ciphertext      []byte         // AES-256-GCM encrypted key
	Nonce           []byte         // 12-byte GCM nonce, unique per encryption
	EncryptedDEK    []byte         // DEK wrapped by KMS master key
	KMSKeyID        string         // which KMS key version wrapped the DEK
	DisplayHint     string         `gorm:"size:8"`                 // last 4 chars
	Status          string         `gorm:"size:16;default:active"` // active | revoked | invalid
	LastUsedAt      *time.Time
	LastValidatedAt *time.Time
}
