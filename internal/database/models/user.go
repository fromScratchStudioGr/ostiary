package models

import "gorm.io/gorm"

// User is an admin-panel operator: the only entity that ever logs in
// and manages consumers, provider keys and gateway API keys.
type User struct {
	gorm.Model
	Username string `gorm:"uniqueIndex;size:64"`
	Password string
	Status   string `gorm:"size:16;default:active"`
}
