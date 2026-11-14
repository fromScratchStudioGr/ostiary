package models

import "time"

type APIKey struct {
	ID          uint `gorm:"primaryKey"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ConsumerID  uint     `gorm:"index"`
	Consumer    Consumer `gorm:"foreignKey:ConsumerID"`
	Name        string   `gorm:"size:64"`
	KeyHash     string   `gorm:"uniqueIndex;size:64"`
	DisplayHint string   `gorm:"size:8"`
	Status      string   `gorm:"size:16;default:active;check:status IN ('active','revoked')"`
	ExpiresAt   time.Time
	LastUsedAt  *time.Time
}
