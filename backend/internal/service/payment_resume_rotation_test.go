//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 规则：轮换后，旧主密钥签发的续签 token 在历史密钥仍配置时继续可校验；新 token 只用新主密钥签。
func TestPaymentResumeServiceVerifiesTokensSignedBeforeRotation(t *testing.T) {
	t.Setenv(paymentResumeSigningKeyEnv, "")
	oldKey := []byte("old-key-old-key-old-key-old-key!")
	newKey := []byte("new-key-new-key-new-key-new-key!")

	before := newLegacyAwarePaymentResumeService(oldKey)
	token, err := before.CreateToken(ResumeTokenClaims{OrderID: 42, UserID: 7})
	require.NoError(t, err)

	after := newLegacyAwarePaymentResumeService(newKey, oldKey)
	claims, err := after.ParseToken(token)
	require.NoError(t, err)
	require.Equal(t, int64(42), claims.OrderID)

	retired := newLegacyAwarePaymentResumeService(newKey)
	_, err = retired.ParseToken(token)
	require.Error(t, err, "once the previous key is removed, tokens it signed are rejected")

	fresh, err := after.CreateToken(ResumeTokenClaims{OrderID: 43})
	require.NoError(t, err)
	_, err = before.ParseToken(fresh)
	require.Error(t, err, "new tokens are signed with the new primary only")
}

func TestResolvePaymentResumeSigningKeysIncludesPreviousKeys(t *testing.T) {
	oldKey := []byte("old-key-old-key-old-key-old-key!")
	newKey := []byte("new-key-new-key-new-key-new-key!")
	explicit := "explicit-signing-key-explicit-signing-key"

	t.Setenv(paymentResumeSigningKeyEnv, "")
	signing, fallbacks := resolvePaymentResumeSigningKeys(newKey, [][]byte{oldKey})
	require.Equal(t, newKey, signing)
	require.Equal(t, [][]byte{oldKey}, fallbacks)

	signing, fallbacks = resolvePaymentResumeSigningKeys(nil, [][]byte{oldKey})
	require.Nil(t, signing, "no primary means no signing, whatever previous keys exist")
	require.Nil(t, fallbacks)

	t.Setenv(paymentResumeSigningKeyEnv, explicit)
	signing, fallbacks = resolvePaymentResumeSigningKeys(newKey, [][]byte{oldKey})
	require.Equal(t, []byte(explicit), signing)
	require.Equal(t, [][]byte{newKey, oldKey}, fallbacks, "explicit signing key keeps the ring keys as verify fallbacks")
}
