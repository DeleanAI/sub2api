package payment

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// AES256KeySize is the required key length (in bytes) for AES-256-GCM.
const AES256KeySize = 32

// ErrProviderConfigUnreadable 表示存储的渠道配置既不是明文 JSON，也不是用任一
// 已配置密钥能解开的旧格式密文。
var ErrProviderConfigUnreadable = errors.New("payment provider config is neither plaintext JSON nor legacy ciphertext decryptable with a configured key")

// DecodeProviderConfig 是读取渠道配置的唯一入口：先按明文 JSON 解析，失败再按
// 旧格式（iv:authTag:ciphertext）用每一把密钥（主密钥在前，历史密钥在后）尝试。
//
// legacy=true 表示这条记录还是升级前的密文，读到以后应尽快改写成明文
// （`sub2api encryption-key rotate` 会批量做）。两种形态都解不开时返回错误而不是
// 空配置：把"密钥不对"当成"没配置"会让管理员去错误的地方排查。
func DecodeProviderConfig(stored string, keys [][]byte) (cfg map[string]string, legacy bool, err error) {
	if stored == "" {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(stored), &cfg); err == nil {
		return cfg, false, nil
	}
	var lastErr error
	tried := 0
	for _, key := range keys {
		if len(key) != AES256KeySize {
			continue
		}
		tried++
		//nolint:staticcheck // SA1019: 旧格式只剩读取路径，这里就是那条路径
		plaintext, err := Decrypt(stored, key)
		if err != nil {
			lastErr = err
			continue
		}
		if err := json.Unmarshal([]byte(plaintext), &cfg); err != nil {
			lastErr = fmt.Errorf("legacy ciphertext decrypted but is not JSON: %w", err)
			continue
		}
		return cfg, true, nil
	}
	if tried == 0 {
		return nil, false, fmt.Errorf("%w: no encryption key configured (stored_len=%d)", ErrProviderConfigUnreadable, len(stored))
	}
	return nil, false, fmt.Errorf("%w: tried %d key(s), last error: %v (stored_len=%d)", ErrProviderConfigUnreadable, tried, lastErr, len(stored))
}

// Encrypt encrypts plaintext using AES-256-GCM with the given 32-byte key.
// The output format is "iv:authTag:ciphertext" where each component is base64-encoded,
// matching the Node.js crypto.ts format for cross-compatibility.
//
// Deprecated: payment provider configs are now stored as plaintext JSON.
// This function is kept only for seeding legacy ciphertext in tests and for
// the transitional Decrypt fallback. Scheduled for removal after all live
// deployments complete migration by re-saving their configs.
func Encrypt(plaintext string, key []byte) (string, error) {
	if len(key) != AES256KeySize {
		return "", fmt.Errorf("encryption key must be %d bytes, got %d", AES256KeySize, len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize()) // 12 bytes for GCM
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	// Seal appends the ciphertext + auth tag
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)

	// Split sealed into ciphertext and auth tag (last 16 bytes)
	tagSize := gcm.Overhead()
	ciphertext := sealed[:len(sealed)-tagSize]
	authTag := sealed[len(sealed)-tagSize:]

	// Format: iv:authTag:ciphertext (all base64)
	return fmt.Sprintf("%s:%s:%s",
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(authTag),
		base64.StdEncoding.EncodeToString(ciphertext),
	), nil
}

// Decrypt decrypts a ciphertext string produced by Encrypt.
// The input format is "iv:authTag:ciphertext" where each component is base64-encoded.
//
// Deprecated: payment provider configs are now stored as plaintext JSON.
// This function remains only as a read-path fallback for pre-migration
// ciphertext records. Scheduled for removal once all deployments re-save
// their provider configs through the admin UI.
func Decrypt(ciphertext string, key []byte) (string, error) {
	if len(key) != AES256KeySize {
		return "", fmt.Errorf("encryption key must be %d bytes, got %d", AES256KeySize, len(key))
	}

	parts := strings.SplitN(ciphertext, ":", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid ciphertext format: expected iv:authTag:ciphertext")
	}

	nonce, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode IV: %w", err)
	}

	authTag, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode auth tag: %w", err)
	}

	encrypted, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}

	// Reconstruct the sealed data: ciphertext + authTag
	sealed := append(encrypted, authTag...)

	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plaintext), nil
}
