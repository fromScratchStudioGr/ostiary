package models

import "gorm.io/gorm"

// Consumer is the entity that's assigned ProviderKey access and gateway APIKeys.
// It never authenticates itself, it only uses the assigned APIKeys.
type Consumer struct {
	gorm.Model
	Name   string `gorm:"size:64"`
	Status string `gorm:"size:16;default:active"`
}
