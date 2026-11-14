package models

import "gorm.io/gorm"

type Usage struct {
	gorm.Model
	ConsumerID                       uint
	Consumer                         Consumer `gorm:"foreignKey:ConsumerID"`
	ProviderKeyID                    uint
	ProviderKey                      ProviderKey `gorm:"foreignKey:ProviderKeyID"`
	CompletionTokens                 int
	CompletionTokensCost             float64
	PromptCacheHitTokens             int
	PromptCacheHitTokensCost         float64
	PromptCacheMissTokens            int
	PromptCacheMissTokensCost        float64
	PromptCacheWriteLevel1Tokens     int
	PromptCacheWriteLevel1TokensCost float64
	PromptCacheWriteLevel2Tokens     int
	PromptCacheWriteLevel2TokensCost float64
}
