package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/secretcipher"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

const (
	cliOldKey = "3333333333333333333333333333333333333333333333333333333333333333"
	cliNewKey = "4444444444444444444444444444444444444444444444444444444444444444"
)

// cliTestEnv 让真正的 config.LoadForBootstrap 跑起来：命令的校验全部走配置加载。
// keyFile 把一把密钥写进临时文件并返回路径。命令只从文件/stdin 读密钥，
// 不接受 flag 值——argv 在 /proc/<pid>/cmdline 里对同机任何用户可见。
func keyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte(key+"\n"), 0o600))
	return path
}

func cliTestEnv(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATA_DIR", "")
	t.Setenv("SERVER_MODE", "release")
	t.Setenv("JWT_SECRET", strings.Repeat("j", 32))
	t.Setenv("TOTP_ENCRYPTION_KEY", "")
	t.Setenv("TOTP_ENCRYPTION_KEY_PREVIOUS", "")
}

// cliMemDB 是一个共享缓存的内存 SQLite 库。命令会 Close 它拿到的 client，而内存库在
// 最后一个连接关闭时消失，所以这里始终保持一条锚连接，让命令前后的读写落在同一个库上。
type cliMemDB struct {
	dsn string
}

func newCLIMemDB(t *testing.T) *cliMemDB {
	t.Helper()
	m := &cliMemDB{dsn: fmt.Sprintf("file:%s?mode=memory&cache=shared&_fk=1", strings.ReplaceAll(t.Name(), "/", "_"))}
	anchor := m.open(t)
	t.Cleanup(func() { _ = anchor.Close() })
	return m
}

func (m *cliMemDB) open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", m.dsn)
	require.NoError(t, err)
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	return db
}

// client 返回一个新的 ent client；调用方（或被测命令）负责 Close。
func (m *cliMemDB) client(t *testing.T) *ent.Client {
	t.Helper()
	c := enttest.NewClient(t, enttest.WithOptions(ent.Driver(entsql.OpenDB(dialect.SQLite, m.open(t)))))
	ensureNonEntEncryptedStoreTables(t, m)
	return c
}

// ensureNonEntEncryptedStoreTables 遍历 repository.EncryptedStores，为其中没有 ent
// schema 的表（如只由 SQL 迁移创建的 plugins）建出轮换需要的最小结构。
//
// 遍历声明而不是写死表名：新增一处裸表密文存储时，这里当天就跟上，而不是等到
// 轮换命令在那条 store 上炸出 "no such table"。
func ensureNonEntEncryptedStoreTables(t *testing.T, m *cliMemDB) {
	t.Helper()
	db := m.open(t)
	defer func() { _ = db.Close() }()
	for _, store := range repository.EncryptedStores {
		var exists int
		err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, store.Table()).Scan(&exists)
		require.NoError(t, err)
		if exists > 0 {
			continue
		}
		idType := "INTEGER"
		if store.IDIsString() {
			idType = "TEXT"
		}
		_, err = db.Exec(fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %q (%q %s PRIMARY KEY, %q TEXT NOT NULL DEFAULT '')`,
			store.Table(), store.IDColumn(), idType, store.Column()))
		require.NoError(t, err, "create non-ent table for encrypted store %s", store.Name)
	}
}

func cliDeps(t *testing.T, m *cliMemDB) encryptionKeyDeps {
	t.Helper()
	return encryptionKeyDeps{
		loadConfig: config.LoadForBootstrap,
		openDB: func(cfg *config.Config) (*ent.Client, error) {
			require.NotNil(t, m, "openDB must not be reached when validation fails")
			return m.client(t), nil
		},
	}
}

func seedCLIUser(t *testing.T, client *ent.Client, ring *secretcipher.Ring, email, secret string) int64 {
	t.Helper()
	ct, err := ring.Encrypt(secret)
	require.NoError(t, err)
	u, err := client.User.Create().SetEmail(email).SetPasswordHash("hash").SetRole(service.RoleUser).SetStatus(service.StatusActive).
		SetTotpSecretEncrypted(ct).Save(context.Background())
	require.NoError(t, err)
	return u.ID
}

func TestEncryptionKeyCommand_UsageAndDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runEncryptionKeyCommandWith(nil, &out, &errOut, cliDeps(t, nil))
	require.ErrorContains(t, err, "missing subcommand")
	require.Contains(t, errOut.String(), "Usage: sub2api encryption-key rotate")

	out.Reset()
	errOut.Reset()
	require.NoError(t, runEncryptionKeyCommandWith([]string{"help"}, &out, &errOut, cliDeps(t, nil)))
	require.Contains(t, out.String(), "TOTP_ENCRYPTION_KEY_PREVIOUS")

	err = runEncryptionKeyCommandWith([]string{"frobnicate"}, &out, &errOut, cliDeps(t, nil))
	require.ErrorContains(t, err, `unknown encryption-key subcommand "frobnicate"`)

	// 注册表里确实有这个命令，且 help 会列出它。
	require.Contains(t, subcommandNames(), "encryption-key")
}

func TestEncryptionKeyRotate_RejectsBadInvocations(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		argsFn  func(*testing.T) []string
		env     map[string]string
		wantErr string
	}{
		{name: "same_old_and_new_flag", argsFn: func(t *testing.T) []string {
			return []string{"--old-key-file", keyFile(t, cliOldKey), "--new-key-file", keyFile(t, cliOldKey)}
		}, wantErr: "same key"},
		{name: "positional_args", args: []string{"extra"}, wantErr: "unexpected arguments"},
		{name: "unknown_flag", args: []string{"--bogus"}, wantErr: "flag provided but not defined"},
		{name: "no_primary_in_release", wantErr: "totp.encryption_key is required when server.mode=release"},
		{name: "no_previous_key", env: map[string]string{"TOTP_ENCRYPTION_KEY": cliNewKey}, wantErr: "no key to rotate from"},
		{name: "previous_equals_primary_via_env", env: map[string]string{"TOTP_ENCRYPTION_KEY": cliNewKey, "TOTP_ENCRYPTION_KEY_PREVIOUS": cliNewKey}, wantErr: "duplicates the primary key"},
		{name: "invalid_new_key_flag", argsFn: func(t *testing.T) []string {
			return []string{"--new-key-file", keyFile(t, "zz"), "--old-key-file", keyFile(t, cliOldKey)}
		}, wantErr: "totp.encryption_key"},
		{name: "missing_key_file", args: []string{"--new-key-file", "/nonexistent/key"}, wantErr: "--new-key-file"},
		{name: "empty_key_file", argsFn: func(t *testing.T) []string { return []string{"--new-key-file", keyFile(t, "")} }, wantErr: "file is empty"},
		{name: "auto_generated_primary_in_debug", env: map[string]string{"SERVER_MODE": "debug"}, argsFn: func(t *testing.T) []string { return []string{"--old-key-file", keyFile(t, cliOldKey)} }, wantErr: "auto-generated key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cliTestEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			args := tt.args
			if tt.argsFn != nil {
				args = tt.argsFn(t)
			}
			var out, errOut bytes.Buffer
			err := runEncryptionKeyCommandWith(append([]string{"rotate"}, args...), &out, &errOut, cliDeps(t, nil))
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// 规则：flags 覆盖环境变量，命令走真实配置加载，重写存量密文并更新指纹；可重复执行。
func TestEncryptionKeyRotate_EndToEndAndIdempotent(t *testing.T) {
	cliTestEnv(t)
	ctx := context.Background()
	oldRing, err := secretcipher.NewRing(cliOldKey, nil)
	require.NoError(t, err)
	newRing, err := secretcipher.NewRing(cliNewKey, []string{cliOldKey})
	require.NoError(t, err)

	m := newCLIMemDB(t)
	seed := m.client(t)
	userID := seedCLIUser(t, seed, oldRing, "rotate@example.com", "JBSWY3DPEHPK3PXP")
	require.NoError(t, repository.StoreEncryptionKeyFingerprint(ctx, seed, oldRing.PrimaryFingerprint()))
	require.NoError(t, seed.Close())

	storedFingerprint := func() string {
		c := m.client(t)
		defer func() { _ = c.Close() }()
		stored, _, err := repository.ReadEncryptionKeyFingerprint(ctx, c)
		require.NoError(t, err)
		return stored
	}
	storedSecret := func() string {
		c := m.client(t)
		defer func() { _ = c.Close() }()
		u, err := c.User.Get(ctx, userID)
		require.NoError(t, err)
		return *u.TotpSecretEncrypted
	}

	// dry-run：报告但不写。
	var out, errOut bytes.Buffer
	require.NoError(t, runEncryptionKeyCommandWith([]string{"rotate", "--dry-run", "--old-key-file", keyFile(t, cliOldKey), "--new-key-file", keyFile(t, cliNewKey)}, &out, &errOut, cliDeps(t, m)))
	require.Contains(t, out.String(), "mode=dry-run")
	require.Contains(t, out.String(), "dry-run: nothing was written")
	require.Equal(t, oldRing.PrimaryFingerprint(), storedFingerprint())
	id, _ := secretcipher.KeyIDOf(storedSecret())
	require.Equal(t, oldRing.PrimaryKeyID(), id, "dry-run must not rewrite rows")

	// 正式执行。
	out.Reset()
	require.NoError(t, runEncryptionKeyCommandWith([]string{"rotate", "--old-key-file", keyFile(t, cliOldKey), "--new-key-file", keyFile(t, cliNewKey)}, &out, &errOut, cliDeps(t, m)))
	require.Contains(t, out.String(), "mode=execute")
	require.Contains(t, out.String(), "stored fingerprint updated to primary key "+newRing.PrimaryKeyID())
	require.Equal(t, newRing.PrimaryFingerprint(), storedFingerprint())
	secret := storedSecret()
	id, _ = secretcipher.KeyIDOf(secret)
	require.Equal(t, newRing.PrimaryKeyID(), id)
	plain, err := newRing.Decrypt(secret)
	require.NoError(t, err)
	require.Equal(t, "JBSWY3DPEHPK3PXP", plain)

	// 再跑一次：没有行需要重写，仍然成功。
	out.Reset()
	require.NoError(t, runEncryptionKeyCommandWith([]string{"rotate", "--old-key-file", keyFile(t, cliOldKey), "--new-key-file", keyFile(t, cliNewKey)}, &out, &errOut, cliDeps(t, m)))
	require.Equal(t, secret, storedSecret(), "already-current rows are left untouched")
}

func TestEncryptionKeyRotate_FailuresExitNonZeroAndKeepFingerprint(t *testing.T) {
	cliTestEnv(t)
	t.Setenv("TOTP_ENCRYPTION_KEY", cliNewKey)
	t.Setenv("TOTP_ENCRYPTION_KEY_PREVIOUS", cliOldKey)
	ctx := context.Background()
	oldRing, err := secretcipher.NewRing(cliOldKey, nil)
	require.NoError(t, err)
	lostRing, err := secretcipher.NewRing(strings.Repeat("5", 64), nil)
	require.NoError(t, err)

	m := newCLIMemDB(t)
	seed := m.client(t)
	seedCLIUser(t, seed, oldRing, "ok@example.com", "secret-ok")
	lostID := seedCLIUser(t, seed, lostRing, "lost@example.com", "secret-lost")
	require.NoError(t, repository.StoreEncryptionKeyFingerprint(ctx, seed, oldRing.PrimaryFingerprint()))
	require.NoError(t, seed.Close())

	var out, errOut bytes.Buffer
	err = runEncryptionKeyCommandWith([]string{"rotate"}, &out, &errOut, cliDeps(t, m))
	require.ErrorContains(t, err, "1 row(s) could not be rotated")
	require.Contains(t, out.String(), fmt.Sprintf("failed: users[%d]:", lostID))
	require.Contains(t, out.String(), "stored fingerprint NOT updated")

	verify := m.client(t)
	defer func() { _ = verify.Close() }()
	stored, _, err := repository.ReadEncryptionKeyFingerprint(ctx, verify)
	require.NoError(t, err)
	require.Equal(t, oldRing.PrimaryFingerprint(), stored)
}
