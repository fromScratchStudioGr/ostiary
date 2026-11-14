package models

import "time"

type UserProviderAccess struct {
	ConsumerID    uint `gorm:"primaryKey"`
	ProviderKeyID uint `gorm:"primaryKey"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Consumer      Consumer    `gorm:"foreignKey:ConsumerID"`
	ProviderKey   ProviderKey `gorm:"foreignKey:ProviderKeyID"`
	Status        string      `gorm:"size:16;default:active"`
	UsageLimit    float64
	TotalCost     float64
}
