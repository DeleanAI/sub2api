package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
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
	return enttest.NewClient(t, enttest.WithOptions(ent.Driver(entsql.OpenDB(dialect.SQLite, m.open(t)))))
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
		env     map[string]string
		wantErr string
	}{
		{name: "same_old_and_new_flag", args: []string{"--old-key", cliOldKey, "--new-key", cliOldKey}, wantErr: "same key"},
		{name: "positional_args", args: []string{"extra"}, wantErr: "unexpected arguments"},
		{name: "unknown_flag", args: []string{"--bogus"}, wantErr: "flag provided but not defined"},
		{name: "no_primary_in_release", wantErr: "totp.encryption_key is required when server.mode=release"},
		{name: "no_previous_key", env: map[string]string{"TOTP_ENCRYPTION_KEY": cliNewKey}, wantErr: "no key to rotate from"},
		{name: "previous_equals_primary_via_env", env: map[string]string{"TOTP_ENCRYPTION_KEY": cliNewKey, "TOTP_ENCRYPTION_KEY_PREVIOUS": cliNewKey}, wantErr: "duplicates the primary key"},
		{name: "invalid_new_key_flag", args: []string{"--new-key", "zz", "--old-key", cliOldKey}, wantErr: "totp.encryption_key"},
		{name: "auto_generated_primary_in_debug", env: map[string]string{"SERVER_MODE": "debug"}, args: []string{"--old-key", cliOldKey}, wantErr: "auto-generated key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cliTestEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var out, errOut bytes.Buffer
			err := runEncryptionKeyCommandWith(append([]string{"rotate"}, tt.args...), &out, &errOut, cliDeps(t, nil))
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
	require.NoError(t, runEncryptionKeyCommandWith([]string{"rotate", "--dry-run", "--old-key", cliOldKey, "--new-key", cliNewKey}, &out, &errOut, cliDeps(t, m)))
	require.Contains(t, out.String(), "mode=dry-run")
	require.Contains(t, out.String(), "dry-run: nothing was written")
	require.Equal(t, oldRing.PrimaryFingerprint(), storedFingerprint())
	id, _ := secretcipher.KeyIDOf(storedSecret())
	require.Equal(t, oldRing.PrimaryKeyID(), id, "dry-run must not rewrite rows")

	// 正式执行。
	out.Reset()
	require.NoError(t, runEncryptionKeyCommandWith([]string{"rotate", "--old-key", cliOldKey, "--new-key", cliNewKey}, &out, &errOut, cliDeps(t, m)))
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
	require.NoError(t, runEncryptionKeyCommandWith([]string{"rotate", "--old-key", cliOldKey, "--new-key", cliNewKey}, &out, &errOut, cliDeps(t, m)))
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
