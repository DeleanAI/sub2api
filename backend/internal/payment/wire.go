package payment

import (
	"log/slog"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/secretcipher"
	"github.com/google/wire"
)

// EncryptionKey is a named type for the payment encryption key (AES-256, 32 bytes).
// Using a named type avoids Wire ambiguity with other []byte parameters.
type EncryptionKey []byte

// PreviousEncryptionKeys 是轮换期间仍然有效的历史密钥（TOTP_ENCRYPTION_KEY_PREVIOUS）。
// 支付这边用它读升级前的旧格式渠道配置密文、校验旧密钥签发的续签 token，
// 让密钥轮换对进行中的支付流程无感。
type PreviousEncryptionKeys [][]byte

// ProvideEncryptionKey derives the payment encryption key from the TOTP encryption key in config.
// When the key is not explicitly configured, nil is returned (payment features that need
// encryption will be disabled). A configured but invalid key ring is a startup error.
func ProvideEncryptionKey(cfg *config.Config) (EncryptionKey, error) {
	ring, ok, err := configuredKeyRing(cfg)
	if err != nil || !ok {
		return nil, err
	}
	return EncryptionKey(ring.Keys()[0]), nil
}

// ProvidePreviousEncryptionKeys 返回历史密钥；密钥未显式配置时为空，与 ProvideEncryptionKey 一致。
func ProvidePreviousEncryptionKeys(cfg *config.Config) (PreviousEncryptionKeys, error) {
	ring, ok, err := configuredKeyRing(cfg)
	if err != nil || !ok {
		return nil, err
	}
	return PreviousEncryptionKeys(ring.Keys()[1:]), nil
}

// configuredKeyRing 只在密钥是运维显式配置时返回密钥环。
// 自动生成的密钥每次重启、每个实例都不同，用它签发的续签 token 或加密的配置
// 会在下一次重启后悄悄失效，所以宁可让支付功能不可用也不用它。
func configuredKeyRing(cfg *config.Config) (*secretcipher.Ring, bool, error) {
	if cfg == nil || strings.TrimSpace(cfg.Totp.EncryptionKey) == "" {
		slog.Warn("payment encryption key not configured — encrypted payment config and resume signing will be unavailable")
		return nil, false, nil
	}
	if !cfg.Totp.EncryptionKeyConfigured {
		slog.Warn("payment encryption/signing key is not explicitly configured; set TOTP_ENCRYPTION_KEY to enable payment resume tokens")
		return nil, false, nil
	}
	ring, err := cfg.Totp.KeyRing()
	if err != nil {
		return nil, false, err
	}
	return ring, true, nil
}

// ProvideRegistry creates an empty payment provider registry.
// Providers are registered at runtime after application startup.
func ProvideRegistry() *Registry {
	return NewRegistry()
}

// ProvideDefaultLoadBalancer creates a DefaultLoadBalancer backed by the ent client.
func ProvideDefaultLoadBalancer(client *dbent.Client, key EncryptionKey, previous PreviousEncryptionKeys) *DefaultLoadBalancer {
	return NewDefaultLoadBalancer(client, []byte(key), previous...)
}

// ProviderSet is the Wire provider set for the payment package.
var ProviderSet = wire.NewSet(
	ProvideEncryptionKey,
	ProvidePreviousEncryptionKeys,
	ProvideRegistry,
	ProvideDefaultLoadBalancer,
	wire.Bind(new(LoadBalancer), new(*DefaultLoadBalancer)),
)
