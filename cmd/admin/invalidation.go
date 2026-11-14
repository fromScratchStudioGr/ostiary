package main

import (
	"context"

	"github.com/fromScratchStudioGr/ostiary/internal/cache"
	"github.com/fromScratchStudioGr/ostiary/internal/database/models"

	"gorm.io/gorm"
)

func consumerKeyHashes(ctx context.Context, db *gorm.DB, consumerID uint) ([]string, error) {
	var keyHashes []string
	err := db.WithContext(ctx).Model(&models.APIKey{}).Where("consumer_id = ?", consumerID).Pluck("key_hash", &keyHashes).Error
	return keyHashes, err
}

func providerKeyKeyHashes(ctx context.Context, db *gorm.DB, providerKeyID uint) ([]string, error) {
	var keyHashes []string
	err := db.WithContext(ctx).Model(&models.APIKey{}).
		Where("consumer_id IN (?)", db.Model(&models.UserProviderAccess{}).Select("consumer_id").Where("provider_key_id = ?", providerKeyID)).
		Pluck("key_hash", &keyHashes).Error
	return keyHashes, err
}

func invalidateConsumerSessions(ctx context.Context, db *gorm.DB, redis *cache.Cache, consumerID uint) error {
	keyHashes, err := consumerKeyHashes(ctx, db, consumerID)
	if err != nil {
		return err
	}
	return redis.InvalidateAPIKeys(ctx, keyHashes)
}

func invalidateProviderKeySessions(ctx context.Context, db *gorm.DB, redis *cache.Cache, providerKeyID uint) error {
	keyHashes, err := providerKeyKeyHashes(ctx, db, providerKeyID)
	if err != nil {
		return err
	}
	return redis.InvalidateAPIKeys(ctx, keyHashes)
}
