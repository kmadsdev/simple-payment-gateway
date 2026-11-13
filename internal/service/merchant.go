package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/url"
	"strings"

	"github.com/kmadsdev/simple-payment-gateway/internal/id"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
)

// API key layout: "sk_test_" + 64 hex characters (256 random bits).
// The first 20 characters are the lookup prefix. Only the SHA-256 of the
// whole key is stored. A slow hash (bcrypt, argon2) is not needed: the key
// is random with 256 bits of entropy, so a brute-force attack on the hash
// is not practical, and a fast hash keeps auth cheap on every request.
const (
	apiKeyPrefix    = "sk_test_"
	apiKeyLen       = len(apiKeyPrefix) + 64
	apiKeyLookupLen = 20
)

// ErrUnauthenticated means the API key is missing, malformed or unknown.
var ErrUnauthenticated = errors.New("invalid API key")

// NewMerchant holds the one-time secrets of a new merchant.
type NewMerchant struct {
	Merchant      store.Merchant
	APIKey        string // shown once, never stored
	WebhookSecret string
}

// CreateMerchant creates a merchant with a new API key. When
// webhookSecret is empty a random one is generated.
func (s *Service) CreateMerchant(ctx context.Context, name, webhookURL, webhookSecret string) (NewMerchant, error) {
	if strings.TrimSpace(name) == "" {
		return NewMerchant{}, invalid("name", "is required")
	}
	if webhookURL != "" {
		u, err := url.Parse(webhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return NewMerchant{}, invalid("webhook_url", "must be an absolute http or https URL")
		}
	}
	if webhookSecret == "" {
		webhookSecret = "whsec_" + id.Secret(24)
	}
	key := apiKeyPrefix + id.Secret(32)
	hash := sha256.Sum256([]byte(key))
	m := store.Merchant{
		ID: id.New("mer"), Name: name, APIKeyPrefix: key[:apiKeyLookupLen], APIKeyHash: hash[:],
		WebhookURL: webhookURL, WebhookSecret: webhookSecret,
	}
	if err := s.Store.CreateMerchant(ctx, m); err != nil {
		return NewMerchant{}, err
	}
	return NewMerchant{Merchant: m, APIKey: key, WebhookSecret: webhookSecret}, nil
}

// Authenticate finds the merchant of an API key. The hash comparison is
// constant time.
func (s *Service) Authenticate(ctx context.Context, key string) (store.Merchant, error) {
	if len(key) != apiKeyLen || !strings.HasPrefix(key, apiKeyPrefix) {
		return store.Merchant{}, ErrUnauthenticated
	}
	m, err := s.Store.MerchantByKeyPrefix(ctx, key[:apiKeyLookupLen])
	if errors.Is(err, store.ErrNotFound) {
		return store.Merchant{}, ErrUnauthenticated
	}
	if err != nil {
		return store.Merchant{}, err
	}
	got := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare(got[:], m.APIKeyHash) != 1 {
		return store.Merchant{}, ErrUnauthenticated
	}
	return m, nil
}
