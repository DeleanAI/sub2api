package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/securitysecret"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/secretcipher"
)

const (
	securitySecretKeyJWT = "jwt_secret"
	// SecuritySecretKeyEncryptionKeyFingerprint 存的是落库密文主密钥的 sha256 指纹
	// （永远不是密钥本身）。启动时拿它和本实例配置的密钥比对，把"这台实例的
	// 密钥和库里数据用的不是同一把"从随机的认证失败变成一条明确的启动错误。
	SecuritySecretKeyEncryptionKeyFingerprint = "totp_encryption_key_fingerprint"
	securitySecretReadRetryMax                = 5
	securitySecretReadRetryWait               = 10 * time.Millisecond
)

var readRandomBytes = rand.Read

func ensureBootstrapSecrets(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	if client == nil {
		return fmt.Errorf("nil ent client")
	}
	if cfg == nil {
		return fmt.Errorf("nil config")
	}
	if err := ensureJWTSecret(ctx, client, cfg); err != nil {
		return err
	}
	return ensureEncryptionKeyFingerprint(ctx, client, cfg)
}

// ensureEncryptionKeyFingerprint 是跨实例密钥一致性的唯一检查点。
//
// 规则：库里记录的是"加密了存量数据的那把密钥"的指纹；本实例的密钥环里必须
// 有这把密钥。它是主密钥 → 正常；它在历史密钥里 → 轮换进行中，提示执行
// `sub2api encryption-key rotate`；都不是 → release 模式拒绝启动，debug 模式
// 记错误继续。指纹不存在时记录当前主密钥的指纹（升级前的数据默认就是它加密的）。
//
// 自动生成的密钥（仅 debug 模式）每个进程都不同，记录它只会在下次启动制造一次
// 必然的不匹配，所以跳过并明说。
func ensureEncryptionKeyFingerprint(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	if !cfg.Totp.EncryptionKeyConfigured || strings.TrimSpace(cfg.Totp.EncryptionKey) == "" {
		slog.Warn("encryption key fingerprint check skipped: totp.encryption_key is auto-generated for this process only, nothing to compare against")
		return nil
	}
	ring, err := cfg.Totp.KeyRing()
	if err != nil {
		return err
	}
	stored, found, err := ReadEncryptionKeyFingerprint(ctx, client)
	if err != nil {
		return fmt.Errorf("read encryption key fingerprint: %w", err)
	}
	if !found {
		stored, err = createSecuritySecretIfAbsent(ctx, client, SecuritySecretKeyEncryptionKeyFingerprint, ring.PrimaryFingerprint())
		if err != nil {
			return fmt.Errorf("record encryption key fingerprint: %w", err)
		}
		if stored == ring.PrimaryFingerprint() {
			slog.Info("encryption key fingerprint recorded: existing encrypted data is assumed to use this key",
				"encryption_key.fingerprint", shortFingerprint(stored),
				"key_id", ring.PrimaryKeyID())
			return nil
		}
		// 另一实例抢先写入了别的指纹：按已存在的情况处理。
	}
	keyID, primary, known := ring.KeyIDByFingerprint(stored)
	switch {
	case known && primary:
		slog.Info("encryption key matches the key that encrypted the stored data",
			"encryption_key.fingerprint", shortFingerprint(stored),
			"key_id", ring.PrimaryKeyID())
		return nil
	case known:
		slog.Warn("encryption key rotation pending: stored data was encrypted with a key that is now in TOTP_ENCRYPTION_KEY_PREVIOUS; "+
			"run `sub2api encryption-key rotate` to re-encrypt it under the primary key before removing the previous key",
			"encryption_key.fingerprint", shortFingerprint(stored),
			"stored_key_id", keyID,
			"primary_key_id", ring.PrimaryKeyID())
		return nil
	}
	msg := fmt.Sprintf("encryption key mismatch: this instance is configured with key %s (previous: %s) but the data in this database was encrypted with key fingerprint %s. "+
		"Either set the TOTP_ENCRYPTION_KEY the other instances use, or — to rotate — keep the old key in TOTP_ENCRYPTION_KEY_PREVIOUS, "+
		"restart, and run `sub2api encryption-key rotate`",
		ring.PrimaryKeyID(), strings.Join(ring.PreviousKeyIDs(), ","), secretcipher.KeyIDOfFingerprint(stored))
	if cfg.Server.Mode == "release" {
		return errors.New(msg)
	}
	slog.Error(msg+" (continuing because server.mode=debug; expect decrypt failures on existing data)",
		"encryption_key.fingerprint", shortFingerprint(stored))
	return nil
}

// shortFingerprint 返回指纹前 8 个字符供日志使用；库里的值经过长度校验，但日志
// 代码不应因为一条被手改过的行而 panic。
func shortFingerprint(fingerprint string) string {
	const n = 8
	if len(fingerprint) <= n {
		return fingerprint
	}
	return fingerprint[:n]
}

// ReadEncryptionKeyFingerprint 读取库里记录的主密钥指纹；没有记录时 found=false。
func ReadEncryptionKeyFingerprint(ctx context.Context, client *ent.Client) (fingerprint string, found bool, err error) {
	stored, err := client.SecuritySecret.Query().Where(securitysecret.KeyEQ(SecuritySecretKeyEncryptionKeyFingerprint)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(stored.Value), true, nil
}

// StoreEncryptionKeyFingerprint 覆盖写入主密钥指纹。只有轮换命令在全部存量密文
// 重写成功后才调用它；启动路径只会在指纹不存在时写入，不会覆盖。
func StoreEncryptionKeyFingerprint(ctx context.Context, client *ent.Client, fingerprint string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if len(fingerprint) < 32 {
		return fmt.Errorf("fingerprint %q is too short to be a sha256 hex", fingerprint)
	}
	return client.SecuritySecret.Create().
		SetKey(SecuritySecretKeyEncryptionKeyFingerprint).
		SetValue(fingerprint).
		OnConflictColumns(securitysecret.FieldKey).
		UpdateValue().
		UpdateUpdatedAt().
		Exec(ctx)
}

func ensureJWTSecret(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	cfg.JWT.Secret = strings.TrimSpace(cfg.JWT.Secret)
	if cfg.JWT.Secret != "" {
		storedSecret, err := createSecuritySecretIfAbsent(ctx, client, securitySecretKeyJWT, cfg.JWT.Secret)
		if err != nil {
			return fmt.Errorf("persist jwt secret: %w", err)
		}
		if storedSecret != cfg.JWT.Secret {
			log.Println("Warning: configured JWT secret mismatches persisted value; using persisted secret for cross-instance consistency.")
		}
		cfg.JWT.Secret = storedSecret
		return nil
	}

	secret, created, err := getOrCreateGeneratedSecuritySecret(ctx, client, securitySecretKeyJWT, 32)
	if err != nil {
		return fmt.Errorf("ensure jwt secret: %w", err)
	}
	cfg.JWT.Secret = secret

	if created {
		log.Println("Warning: JWT secret auto-generated and persisted to database. Consider rotating to a managed secret for production.")
	}
	return nil
}

func getOrCreateGeneratedSecuritySecret(ctx context.Context, client *ent.Client, key string, byteLength int) (string, bool, error) {
	existing, err := client.SecuritySecret.Query().Where(securitysecret.KeyEQ(key)).Only(ctx)
	if err == nil {
		value := strings.TrimSpace(existing.Value)
		if len([]byte(value)) < 32 {
			return "", false, fmt.Errorf("stored secret %q must be at least 32 bytes", key)
		}
		return value, false, nil
	}
	if !ent.IsNotFound(err) {
		return "", false, err
	}

	generated, err := generateHexSecret(byteLength)
	if err != nil {
		return "", false, err
	}

	if err := client.SecuritySecret.Create().
		SetKey(key).
		SetValue(generated).
		OnConflictColumns(securitysecret.FieldKey).
		DoNothing().
		Exec(ctx); err != nil {
		if !isSQLNoRowsError(err) {
			return "", false, err
		}
	}

	stored, err := querySecuritySecretWithRetry(ctx, client, key)
	if err != nil {
		return "", false, err
	}
	value := strings.TrimSpace(stored.Value)
	if len([]byte(value)) < 32 {
		return "", false, fmt.Errorf("stored secret %q must be at least 32 bytes", key)
	}
	return value, value == generated, nil
}

func createSecuritySecretIfAbsent(ctx context.Context, client *ent.Client, key, value string) (string, error) {
	value = strings.TrimSpace(value)
	if len([]byte(value)) < 32 {
		return "", fmt.Errorf("secret %q must be at least 32 bytes", key)
	}

	if err := client.SecuritySecret.Create().
		SetKey(key).
		SetValue(value).
		OnConflictColumns(securitysecret.FieldKey).
		DoNothing().
		Exec(ctx); err != nil {
		if !isSQLNoRowsError(err) {
			return "", err
		}
	}

	stored, err := querySecuritySecretWithRetry(ctx, client, key)
	if err != nil {
		return "", err
	}
	storedValue := strings.TrimSpace(stored.Value)
	if len([]byte(storedValue)) < 32 {
		return "", fmt.Errorf("stored secret %q must be at least 32 bytes", key)
	}
	return storedValue, nil
}

func querySecuritySecretWithRetry(ctx context.Context, client *ent.Client, key string) (*ent.SecuritySecret, error) {
	var lastErr error
	for attempt := 0; attempt <= securitySecretReadRetryMax; attempt++ {
		stored, err := client.SecuritySecret.Query().Where(securitysecret.KeyEQ(key)).Only(ctx)
		if err == nil {
			return stored, nil
		}
		if !isSecretNotFoundError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == securitySecretReadRetryMax {
			break
		}

		timer := time.NewTimer(securitySecretReadRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func isSecretNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return ent.IsNotFound(err) || isSQLNoRowsError(err)
}

func isSQLNoRowsError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no rows in result set")
}

func generateHexSecret(byteLength int) (string, error) {
	if byteLength <= 0 {
		byteLength = 32
	}
	buf := make([]byte, byteLength)
	if _, err := readRandomBytes(buf); err != nil {
		return "", fmt.Errorf("generate random secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
