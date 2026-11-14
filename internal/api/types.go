package api

import "time"

// UserRequest creates an admin-panel operator (User). Admin-only.
type UserRequest struct {
	Username        string `json:"username" validate:"required"`
	Password        string `json:"password" validate:"required"`
	PasswordConfirm string `json:"password_confirm" validate:"required"`
}

type MeResponse struct {
	Username string `json:"username"`
}

type UpdateUserPasswordRequest struct {
	UserId          uint   `json:"userId" validate:"required"`
	Password        string `json:"password" validate:"required"`
	PasswordConfirm string `json:"passwordConfirm" validate:"required"`
}

type UserResponse struct {
	ID        uint      `json:"id"`
	Username  string    `json:"username"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreateConsumerRequest creates a consumer: the entity that gets assigned
// provider-key access and gateway API keys. Admin-only.
type CreateConsumerRequest struct {
	Name string `json:"name" validate:"required"`
}

type ConsumerResponse struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

type UpdateConsumerStatusRequest struct {
	ConsumerId uint   `json:"id" validate:"required"`
	Status     string `json:"status" validate:"required,oneof=active revoked"`
}

type ProviderKeyConsumerResponse struct {
	ID         uint    `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	TotalCost  float64 `json:"totalCost"`
	UsageLimit float64 `json:"usageLimit"`
}

type ConsumerProviderKeyResponse struct {
	ID         uint    `json:"id"`
	Provider   string  `json:"provider"`
	Key        string  `json:"key"`
	Status     string  `json:"status"`
	TotalCost  float64 `json:"totalCost"`
	UsageLimit float64 `json:"usageLimit"`
}

type ProviderKeyResponse struct {
	ID           uint       `json:"id"`
	Provider     string     `json:"provider"`
	Key          string     `json:"key"`
	Status       string     `json:"status"`
	UserCount    int64      `json:"userCount"`
	LastUsedAt   *time.Time `json:"lastUsedAt"`
	RegisteredAt time.Time  `json:"registeredAt"`
}

type RewrapProviderKeysResponse struct {
	Rewrapped int    `json:"rewrapped"`
	Failed    int    `json:"failed"`
	FailedIDs []uint `json:"failedIds"`
}

type CreateProviderKeyRequest struct {
	Provider string `json:"provider" validate:"required"`
	ApiKey   string `json:"apiKey" validate:"required"`
}

type AssignProviderKeyRequest struct {
	ConsumerId    uint    `json:"consumerId" validate:"required"`
	ProviderKeyId uint    `json:"providerKeyId" validate:"required"`
	UsageLimit    float64 `json:"usageLimit" validate:"gte=0"`
}

type UpdateProviderKeyStatusRequest struct {
	ProviderKeyId uint   `json:"providerKeyId" validate:"required"`
	Status        string `json:"status" validate:"required,oneof=active revoked"`
}

type UpdateProviderKeyConsumerStatusRequest struct {
	ProviderKeyId uint   `json:"providerKeyId" validate:"required"`
	ConsumerId    uint   `json:"consumerId" validate:"required"`
	Status        string `json:"status" validate:"required,oneof=active revoked"`
}

type DeleteProviderKeyConsumerRequest struct {
	ProviderKeyId uint `json:"providerKeyId" validate:"required"`
	ConsumerId    uint `json:"consumerId" validate:"required"`
}

type UpdateProviderKeyConsumerLimitRequest struct {
	ProviderKeyId uint    `json:"providerKeyId" validate:"required"`
	ConsumerId    uint    `json:"consumerId" validate:"required"`
	UsageLimit    float64 `json:"usageLimit" validate:"gte=0"`
}

type DailyCostResponse struct {
	Date string  `json:"date"`
	Cost float64 `json:"cost"`
}

type UsageSummaryResponse struct {
	TotalCost    float64             `json:"totalCost"`
	TotalTokens  int64               `json:"totalTokens"`
	UserCount    int64               `json:"userCount"`
	KeyCount     int64               `json:"keyCount"`
	CostOverTime []DailyCostResponse `json:"costOverTime"`
}

type DailyTokensResponse struct {
	Date   string `json:"date"`
	Tokens int64  `json:"tokens"`
}

type KeyCostResponse struct {
	ProviderKeyId uint    `json:"providerKeyId"`
	Provider      string  `json:"provider"`
	Cost          float64 `json:"cost"`
}

type UserUsageSummaryResponse struct {
	TotalCost      float64               `json:"totalCost"`
	TotalTokens    int64                 `json:"totalTokens"`
	TokensOverTime []DailyTokensResponse `json:"tokensOverTime"`
	CostByKey      []KeyCostResponse     `json:"costByKey"`
}

type CreateGatewayAPIKeyRequest struct {
	ConsumerId    uint   `json:"consumerId" validate:"required"`
	Name          string `json:"name" validate:"required"`
	ExpiresInDays int    `json:"expiresInDays" validate:"required,gt=0,lte=365"`
}

type CreateGatewayAPIKeyResponse struct {
	ID          uint      `json:"id"`
	Name        string    `json:"name"`
	Key         string    `json:"key"`
	DisplayHint string    `json:"displayHint"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type GatewayAPIKeyResponse struct {
	ID          uint       `json:"id"`
	Name        string     `json:"name"`
	DisplayHint string     `json:"displayHint"`
	Status      string     `json:"status"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
}

type UpdateGatewayAPIKeyStatusRequest struct {
	ID     uint   `json:"id" validate:"required"`
	Status string `json:"status" validate:"required,oneof=active revoked"`
}
