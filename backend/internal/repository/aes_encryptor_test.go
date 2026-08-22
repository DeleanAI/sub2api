//go:build unit

package repository

import (
	"encoding/hex"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/secretcipher"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aesHexKey 构造一个全填充为 b 的 n 字节密钥并以 hex 编码返回。
func aesHexKey(n int, b byte) string {
	raw := make([]byte, n)
	for i := range raw {
		raw[i] = b
	}
	return hex.EncodeToString(raw)
}

func aesTestCfg(keyHex string, previous ...string) *config.Config {
	cfg := &config.Config{Totp: config.TotpConfig{EncryptionKey: keyHex}}
	for i, p := range previous {
		if i > 0 {
			cfg.Totp.EncryptionKeyPrevious += ","
		}
		cfg.Totp.EncryptionKeyPrevious += p
	}
	return cfg
}

func aesEncryptor(t *testing.T, keyHex string, previous ...string) service.SecretEncryptor {
	t.Helper()
	enc, err := NewAESEncryptor(aesTestCfg(keyHex, previous...))
	require.NoError(t, err)
	require.NotNil(t, enc)
	return enc
}

func TestNewAESEncryptor_ValidKey32Bytes(t *testing.T) {
	enc := aesEncryptor(t, aesHexKey(32, 0x01))
	_, ok := enc.(*secretcipher.Ring)
	require.True(t, ok, "the encryptor is the key ring itself; no second cipher implementation exists")
}

// 16 / 24 字节密钥在 AES 体系内合法，但本实现仅接受 AES-256（32 字节）。
func TestNewAESEncryptor_WrongKeyLength(t *testing.T) {
	for _, size := range []int{1, 16, 24, 31, 33, 64} {
		_, err := NewAESEncryptor(aesTestCfg(aesHexKey(size, 0x00)))
		require.Error(t, err, "key size %d", size)
		assert.Contains(t, err.Error(), "32 bytes")
	}
}

func TestNewAESEncryptor_MissingOrInvalidConfig(t *testing.T) {
	for name, keyHex := range map[string]string{
		"empty_key":              "",
		"invalid_hex_odd_length": "abcde",
		"invalid_hex_chars":      "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewAESEncryptor(aesTestCfg(keyHex))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "totp.encryption_key")
		})
	}
}

func TestNewAESEncryptor_RejectsBadPreviousKeys(t *testing.T) {
	_, err := NewAESEncryptor(aesTestCfg(aesHexKey(32, 0x01), "nope"))
	require.ErrorContains(t, err, "previous key #1")

	_, err = NewAESEncryptor(aesTestCfg(aesHexKey(32, 0x01), aesHexKey(32, 0x01)))
	require.ErrorContains(t, err, "duplicates the primary key")
}

func TestAESEncryptor_RoundTripWritesVersionedCiphertext(t *testing.T) {
	enc := aesEncryptor(t, aesHexKey(32, 0x42))
	ct, err := enc.Encrypt("Hello, Sub2API! 你好")
	require.NoError(t, err)
	require.True(t, secretcipher.IsVersioned(ct), "new ciphertext must carry the key id so it can be rotated later")

	got, err := enc.Decrypt(ct)
	require.NoError(t, err)
	assert.Equal(t, "Hello, Sub2API! 你好", got)
}

// 相同密钥构造的两个实例应可互相解密；不同密钥不能，且错误点名 key-id。
func TestAESEncryptor_CrossInstance(t *testing.T) {
	enc1 := aesEncryptor(t, aesHexKey(32, 0xDE))
	enc2 := aesEncryptor(t, aesHexKey(32, 0xDE))
	other := aesEncryptor(t, aesHexKey(32, 0xBB))

	ct, err := enc1.Encrypt("cross-instance roundtrip")
	require.NoError(t, err)

	got, err := enc2.Decrypt(ct)
	require.NoError(t, err)
	assert.Equal(t, "cross-instance roundtrip", got)

	_, err = other.Decrypt(ct)
	require.ErrorIs(t, err, secretcipher.ErrUnknownKeyID)
	id, _ := secretcipher.KeyIDOf(ct)
	require.ErrorContains(t, err, id)
}

// 轮换中间态：新密钥为主、旧密钥在 PREVIOUS，旧密文仍可读，新密文只用新密钥。
func TestAESEncryptor_PreviousKeyDecryptsOldCiphertext(t *testing.T) {
	oldEnc := aesEncryptor(t, aesHexKey(32, 0x01))
	oldCT, err := oldEnc.Encrypt("written before rotation")
	require.NoError(t, err)

	ring := aesEncryptor(t, aesHexKey(32, 0x02), aesHexKey(32, 0x01))
	got, err := ring.Decrypt(oldCT)
	require.NoError(t, err)
	assert.Equal(t, "written before rotation", got)

	newCT, err := ring.Encrypt("written after rotation")
	require.NoError(t, err)
	id, ok := secretcipher.KeyIDOf(newCT)
	require.True(t, ok)
	require.Equal(t, ring.(*secretcipher.Ring).PrimaryKeyID(), id)
	_, err = oldEnc.Decrypt(newCT)
	require.Error(t, err, "the retired key must not be able to read new data")
}
