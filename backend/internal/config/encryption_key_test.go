package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testPreviousEncryptionKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// 规则：release 模式下落库密文密钥是启动前置条件；debug 模式允许每进程自动生成。
func TestLoadEncryptionKeyPolicy(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		key        string
		previous   string
		wantErr    string
		wantConfig bool
	}{
		{name: "release_unset_fails", mode: "release", wantErr: "totp.encryption_key is required when server.mode=release"},
		{name: "release_blank_fails", mode: "release", key: "   ", wantErr: "openssl rand -hex 32"},
		{name: "release_set_ok", mode: "release", key: testEncryptionKey, wantConfig: true},
		{name: "debug_unset_generates", mode: "debug", wantConfig: false},
		{name: "debug_set_ok", mode: "debug", key: testEncryptionKey, wantConfig: true},
		{name: "previous_parsed", mode: "release", key: testEncryptionKey, previous: " " + testPreviousEncryptionKey + " , ", wantConfig: true},
		{name: "previous_equals_primary_rejected", mode: "release", key: testEncryptionKey, previous: testEncryptionKey, wantErr: "duplicates the primary key"},
		{name: "previous_invalid_rejected", mode: "release", key: testEncryptionKey, previous: "zz", wantErr: "previous key #1"},
		{name: "invalid_primary_rejected", mode: "release", key: "abcd", wantErr: "totp.encryption_key"},
		{name: "debug_previous_with_generated_primary", mode: "debug", previous: testPreviousEncryptionKey, wantConfig: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("SERVER_MODE", tt.mode)
			t.Setenv("TOTP_ENCRYPTION_KEY", tt.key)
			t.Setenv("TOTP_ENCRYPTION_KEY_PREVIOUS", tt.previous)

			cfg, err := Load()
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantConfig, cfg.Totp.EncryptionKeyConfigured)
			require.Len(t, cfg.Totp.EncryptionKey, 64, "the effective key is always a 32-byte hex key")
			if tt.wantConfig {
				require.Equal(t, strings.TrimSpace(tt.key), cfg.Totp.EncryptionKey)
			}
			if strings.TrimSpace(tt.previous) != "" {
				require.Equal(t, []string{testPreviousEncryptionKey}, cfg.Totp.PreviousKeys())
			} else {
				require.Empty(t, cfg.Totp.PreviousKeys())
			}
		})
	}
}

// 自动生成的密钥每个进程都不一样：这正是 release 模式必须拒绝它的原因。
func TestLoadEncryptionKeyDebugAutoGenerationIsPerProcess(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SERVER_MODE", "debug")
	t.Setenv("TOTP_ENCRYPTION_KEY", "")

	first, err := Load()
	require.NoError(t, err)
	second, err := Load()
	require.NoError(t, err)
	require.NotEqual(t, first.Totp.EncryptionKey, second.Totp.EncryptionKey)
	require.False(t, first.Totp.EncryptionKeyConfigured)
}

func TestTotpConfigPreviousKeys(t *testing.T) {
	require.Empty(t, TotpConfig{}.PreviousKeys())
	require.Empty(t, TotpConfig{EncryptionKeyPrevious: " , ,"}.PreviousKeys())
	require.Equal(t, []string{"a", "b"}, TotpConfig{EncryptionKeyPrevious: " a ,b,"}.PreviousKeys())
}

// Validate 是密钥环格式校验的接线点：直接构造的 Config 也要过同一条规则。
func TestValidateEncryptionKeyRing(t *testing.T) {
	resetViperWithJWTSecret(t)
	base, err := Load()
	require.NoError(t, err)

	cfg := *base
	cfg.Totp.EncryptionKey = ""
	cfg.Totp.EncryptionKeyPrevious = ""
	require.NoError(t, cfg.Validate(), "an empty key is Validate's business only when previous keys dangle; the release policy lives in load()")

	cfg.Totp.EncryptionKeyPrevious = testPreviousEncryptionKey
	require.ErrorContains(t, cfg.Validate(), "totp.encryption_key_previous is set but totp.encryption_key is empty")

	cfg.Totp.EncryptionKey = testEncryptionKey
	require.NoError(t, cfg.Validate())

	cfg.Totp.EncryptionKeyPrevious = testPreviousEncryptionKey + "," + testPreviousEncryptionKey
	require.ErrorContains(t, cfg.Validate(), "listed twice")
}
