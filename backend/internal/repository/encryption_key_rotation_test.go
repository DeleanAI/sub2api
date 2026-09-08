package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/channelmonitor"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/pkg/secretcipher"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

const (
	rotationOldKeyHex = "1111111111111111111111111111111111111111111111111111111111111111"
	rotationNewKeyHex = "2222222222222222222222222222222222222222222222222222222222222222"
	rotationLostKey   = "9999999999999999999999999999999999999999999999999999999999999999"
	// rotationBigInt 超过 float64 精确表示范围：用来证明 JSON 改写不会把
	// 无关字段经 float64 往返而损坏。
	rotationBigInt = "9007199254740993"
)

func newRotationSQLiteClient(t *testing.T) *dbent.Client {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_fk=1", name))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	// plugins 由 SQL 迁移建表、没有 ent schema，enttest 不会创建它。轮换注册表里
	// 有 plugins.config_encrypted 这一项，缺表会让整轮轮换在"列出候选行"时失败。
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS plugins (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		config_encrypted TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	return client
}

func mustRing(t *testing.T, primary string, previous ...string) *secretcipher.Ring {
	t.Helper()
	ring, err := secretcipher.NewRing(primary, previous)
	require.NoError(t, err)
	return ring
}

func mustEncrypt(t *testing.T, ring *secretcipher.Ring, plaintext string) string {
	t.Helper()
	ct, err := ring.Encrypt(plaintext)
	require.NoError(t, err)
	return ct
}

// legacyCiphertext 模拟升级前无前缀的旧格式密文：v1 前缀去掉后就是旧格式。
func legacyCiphertext(t *testing.T, ring *secretcipher.Ring, plaintext string) string {
	t.Helper()
	ct := mustEncrypt(t, ring, plaintext)
	parts := strings.SplitN(ct, ":", 3)
	require.Len(t, parts, 3)
	return parts[2]
}

// rotationFixture 在每张表里各放一行旧密钥密文、一行主密钥密文，外加若干边界行。
type rotationFixture struct {
	oldUser, newUser, deletedUser, lostUser int64
	oldMonitor, newMonitor                  int64
	oldAccount, newAccount, noSessionAcct   int64
	legacyPayment, plainPayment             int64
}

func seedRotationFixture(t *testing.T, ctx context.Context, client *dbent.Client, old, current *secretcipher.Ring, includeLost bool) rotationFixture {
	t.Helper()
	var f rotationFixture
	newUser := func(email, secret string) int64 {
		u, err := client.User.Create().SetEmail(email).SetPasswordHash("hash").SetRole(service.RoleUser).SetStatus(service.StatusActive).
			SetTotpSecretEncrypted(secret).Save(ctx)
		require.NoError(t, err)
		return u.ID
	}
	f.oldUser = newUser("old@example.com", legacyCiphertext(t, old, "totp-old"))
	f.newUser = newUser("new@example.com", mustEncrypt(t, current, "totp-new"))
	f.deletedUser = newUser("deleted@example.com", mustEncrypt(t, old, "totp-deleted"))
	require.NoError(t, client.User.DeleteOneID(f.deletedUser).Exec(ctx), "soft delete")
	_, err := client.User.Create().SetEmail("none@example.com").SetPasswordHash("hash").SetRole(service.RoleUser).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	if includeLost {
		f.lostUser = newUser("lost@example.com", mustEncrypt(t, mustRing(t, rotationLostKey), "totp-lost"))
	}

	newMonitor := func(name, secret string) int64 {
		m, err := client.ChannelMonitor.Create().SetName(name).SetProvider(channelmonitor.ProviderOpenai).SetAPIMode(service.MonitorAPIModeResponses).
			SetEndpoint("https://api.example.com").SetAPIKeyEncrypted(secret).SetPrimaryModel("gpt-5").SetIntervalSeconds(60).SetCreatedBy(1).Save(ctx)
		require.NoError(t, err)
		return m.ID
	}
	f.oldMonitor = newMonitor("old", mustEncrypt(t, old, "sk-old"))
	f.newMonitor = newMonitor("new", mustEncrypt(t, current, ""))

	mustSetting := func(key, value string) {
		_, err := client.Setting.Create().SetKey(key).SetValue(value).Save(ctx)
		require.NoError(t, err)
	}
	backup, err := json.Marshal(service.BackupS3Config{Endpoint: "https://s3", Bucket: "b", AccessKeyID: "ak", SecretAccessKey: mustEncrypt(t, old, "s3-secret")})
	require.NoError(t, err)
	mustSetting(service.SettingKeyBackupS3Config, string(backup))
	image, err := json.Marshal(service.ImageStorageSettings{Enabled: true, Bucket: "img", SecretAccessKey: mustEncrypt(t, current, "img-secret")})
	require.NoError(t, err)
	mustSetting(service.SettingKeyImageStorageConfig, string(image))
	audit := map[string]any{
		"enabled": true, "config_version": 7, "big": json.Number(rotationBigInt),
		"endpoints": []map[string]any{
			{"id": "ep-old", "name": "old", "token_ciphertext": mustEncrypt(t, old, "tok-old"), "enabled": true},
			{"id": "ep-new", "name": "new", "token_ciphertext": mustEncrypt(t, current, "tok-new"), "enabled": true},
			{"id": "ep-none", "name": "none", "enabled": false},
		},
	}
	auditRaw, err := json.Marshal(audit)
	require.NoError(t, err)
	mustSetting(securityaudit.SettingKeyPromptAuditConfig, string(auditRaw))
	mustSetting("unrelated", "not-json-and-not-ciphertext")

	newAccount := func(name string, extra map[string]any) int64 {
		a, err := client.Account.Create().SetName(name).SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeAPIKey).SetStatus(service.StatusActive).
			SetCredentials(map[string]any{"api_key": "sk-" + name}).SetExtra(extra).Save(ctx)
		require.NoError(t, err)
		return a.ID
	}
	f.oldAccount = newAccount("old", map[string]any{service.OllamaCloudUsageSessionExtraKey: mustEncrypt(t, old, "cookie-old"), "big": json.Number(rotationBigInt), "nested": map[string]any{"keep": true}})
	f.newAccount = newAccount("new", map[string]any{service.OllamaCloudUsageSessionExtraKey: mustEncrypt(t, current, "cookie-new")})
	f.noSessionAcct = newAccount("plain", map[string]any{"crs_account_id": "x"})

	legacyKey := old.Keys()[0]
	legacyCfg, err := payment.Encrypt(`{"appId":"app-1","secret":"s3cr3t"}`, legacyKey) //nolint:staticcheck // 构造升级前的旧密文
	require.NoError(t, err)
	newPayment := func(name, cfg string) int64 {
		p, err := client.PaymentProviderInstance.Create().SetProviderKey(payment.TypeStripe).SetName(name).SetConfig(cfg).SetSupportedTypes("card").SetEnabled(true).Save(ctx)
		require.NoError(t, err)
		return p.ID
	}
	f.legacyPayment = newPayment("legacy", legacyCfg)
	f.plainPayment = newPayment("plain", `{"appId":"app-2"}`)
	return f
}

func storeReport(t *testing.T, report *RotationReport, prefix string) StoreReport {
	t.Helper()
	for _, s := range report.Stores {
		if strings.HasPrefix(s.Store, prefix) {
			return s
		}
	}
	t.Fatalf("no store report with prefix %q in %+v", prefix, report.Stores)
	return StoreReport{}
}

func rawColumn(t *testing.T, ctx context.Context, client *dbent.Client, table, column string, id any) string {
	t.Helper()
	value, found, err := queryNullString(ctx, client, fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", column, table), id)
	require.NoError(t, err)
	require.True(t, found)
	return value
}

func rawSetting(t *testing.T, ctx context.Context, client *dbent.Client, key string) string {
	t.Helper()
	value, found, err := queryNullString(ctx, client, "SELECT value FROM settings WHERE key = ?", key)
	require.NoError(t, err)
	require.True(t, found)
	return value
}

func assertCurrent(t *testing.T, ring *secretcipher.Ring, ciphertext, wantPlain string) {
	t.Helper()
	id, ok := secretcipher.KeyIDOf(ciphertext)
	require.True(t, ok, "ciphertext must be versioned: %q", ciphertext)
	require.Equal(t, ring.PrimaryKeyID(), id)
	plain, err := ring.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, wantPlain, plain)
}

// 规则：每一处落库密文都被重写到主密钥新格式；已经是主密钥的行按 key-id 跳过；
// 全部成功后库里的指纹指向主密钥。
func TestRotateEncryptionKey_RewritesEveryStoreAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := newRotationSQLiteClient(t)
	old := mustRing(t, rotationOldKeyHex)
	ring := mustRing(t, rotationNewKeyHex, rotationOldKeyHex)
	f := seedRotationFixture(t, ctx, client, old, ring, false)
	require.NoError(t, StoreEncryptionKeyFingerprint(ctx, client, old.PrimaryFingerprint()))

	// dry-run：统计与正式执行一致，但什么都不写。
	dry, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{DryRun: true})
	require.NoError(t, err)
	require.True(t, dry.DryRun)
	require.False(t, dry.FingerprintUpdated)
	require.Equal(t, old.PrimaryKeyID(), dry.StoredFingerprintKeyID)
	require.True(t, dry.StoredFingerprintKnown)
	rotated, _, _, failed := dry.Totals()
	require.Equal(t, 0, failed)
	require.Equal(t, 7, rotated, "old user, soft-deleted user, old monitor, backup secret, audit config, old account, legacy payment config")
	oldUserBefore := rawColumn(t, ctx, client, "users", "totp_secret_encrypted", f.oldUser)
	stored, _, err := ReadEncryptionKeyFingerprint(ctx, client)
	require.NoError(t, err)
	require.Equal(t, old.PrimaryFingerprint(), stored, "dry-run must not touch the fingerprint")

	report, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
	require.NoError(t, err)
	require.False(t, report.Failed())
	require.True(t, report.FingerprintUpdated)
	require.Len(t, report.Stores, len(EncryptedStores), "every registered store is visited")

	users := storeReport(t, report, "users.")
	require.Equal(t, StoreReport{Store: users.Store, Rotated: 2, Current: 1}, users, "legacy row + soft-deleted row rotated, current row skipped, row without secret never listed")
	monitors := storeReport(t, report, "channel_monitors.")
	require.Equal(t, StoreReport{Store: monitors.Store, Rotated: 1, Current: 1}, monitors)
	backup := storeReport(t, report, "settings[backup_s3_config]")
	require.Equal(t, StoreReport{Store: backup.Store, Rotated: 1}, backup)
	image := storeReport(t, report, "settings[image_storage_config]")
	require.Equal(t, StoreReport{Store: image.Store, Current: 1}, image)
	audit := storeReport(t, report, "settings[prompt_audit_config]")
	require.Equal(t, StoreReport{Store: audit.Store, Rotated: 1}, audit, "one row holds both an old and a current endpoint token: counts once")
	accounts := storeReport(t, report, "accounts.")
	require.Equal(t, StoreReport{Store: accounts.Store, Rotated: 1, Current: 1}, accounts, "account without the session key is never listed")
	payments := storeReport(t, report, "payment_provider_instances.")
	require.Equal(t, StoreReport{Store: payments.Store, Rotated: 1, Current: 1}, payments)

	require.NotEqual(t, oldUserBefore, rawColumn(t, ctx, client, "users", "totp_secret_encrypted", f.oldUser))
	assertCurrent(t, ring, rawColumn(t, ctx, client, "users", "totp_secret_encrypted", f.oldUser), "totp-old")
	assertCurrent(t, ring, rawColumn(t, ctx, client, "users", "totp_secret_encrypted", f.deletedUser), "totp-deleted")
	assertCurrent(t, ring, rawColumn(t, ctx, client, "channel_monitors", "api_key_encrypted", f.oldMonitor), "sk-old")
	assertCurrent(t, ring, rawColumn(t, ctx, client, "channel_monitors", "api_key_encrypted", f.newMonitor), "")

	var backupCfg service.BackupS3Config
	require.NoError(t, json.Unmarshal([]byte(rawSetting(t, ctx, client, service.SettingKeyBackupS3Config)), &backupCfg))
	assertCurrent(t, ring, backupCfg.SecretAccessKey, "s3-secret")
	require.Equal(t, "ak", backupCfg.AccessKeyID, "sibling fields survive")

	auditRaw := rawSetting(t, ctx, client, securityaudit.SettingKeyPromptAuditConfig)
	require.Contains(t, auditRaw, `"big":`+rotationBigInt, "untouched numbers keep their exact digits")
	var auditDoc struct {
		ConfigVersion int64                           `json:"config_version"`
		Endpoints     []securityaudit.StorageEndpoint `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal([]byte(auditRaw), &auditDoc))
	require.Equal(t, int64(7), auditDoc.ConfigVersion)
	require.Len(t, auditDoc.Endpoints, 3)
	assertCurrent(t, ring, auditDoc.Endpoints[0].TokenCiphertext, "tok-old")
	assertCurrent(t, ring, auditDoc.Endpoints[1].TokenCiphertext, "tok-new")
	require.Empty(t, auditDoc.Endpoints[2].TokenCiphertext)
	require.Equal(t, "not-json-and-not-ciphertext", rawSetting(t, ctx, client, "unrelated"), "settings outside the registry are never touched")

	extraRaw := rawColumn(t, ctx, client, "accounts", "extra", f.oldAccount)
	require.Contains(t, extraRaw, `"big":`+rotationBigInt)
	require.Contains(t, extraRaw, `"nested":{"keep":true}`)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(extraRaw), &extra))
	assertCurrent(t, ring, extra[service.OllamaCloudUsageSessionExtraKey].(string), "cookie-old")

	paymentRaw := rawColumn(t, ctx, client, "payment_provider_instances", "config", f.legacyPayment)
	var paymentCfg map[string]string
	require.NoError(t, json.Unmarshal([]byte(paymentRaw), &paymentCfg), "legacy payment ciphertext becomes plaintext JSON: %s", paymentRaw)
	require.Equal(t, map[string]string{"appId": "app-1", "secret": "s3cr3t"}, paymentCfg)
	require.Equal(t, `{"appId":"app-2"}`, rawColumn(t, ctx, client, "payment_provider_instances", "config", f.plainPayment))

	stored, _, err = ReadEncryptionKeyFingerprint(ctx, client)
	require.NoError(t, err)
	require.Equal(t, ring.PrimaryFingerprint(), stored)

	// 幂等：第二次执行没有任何行需要重写。
	again, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
	require.NoError(t, err)
	rotated, current, _, failed := again.Totals()
	require.Equal(t, 0, rotated)
	require.Equal(t, 0, failed)
	require.Equal(t, 12, current, "3 users + 2 monitors + 3 settings + 2 accounts + 2 payment configs")
	require.True(t, again.FingerprintUpdated)

	// 退役旧密钥后，只有主密钥的环也能读所有数据。
	onlyNew := mustRing(t, rotationNewKeyHex)
	plain, err := onlyNew.Decrypt(rawColumn(t, ctx, client, "users", "totp_secret_encrypted", f.oldUser))
	require.NoError(t, err)
	require.Equal(t, "totp-old", plain)

	var buf strings.Builder
	report.Write(&buf)
	require.Contains(t, buf.String(), "mode=execute")
	require.Contains(t, buf.String(), "stored fingerprint updated to primary key "+ring.PrimaryKeyID())
}

// 规则：解不开的行算失败并逐行点名，其余行照常重写；有失败时指纹不更新。
func TestRotateEncryptionKey_UndecryptableRowsFailLoudlyAndKeepFingerprint(t *testing.T) {
	ctx := context.Background()
	client := newRotationSQLiteClient(t)
	old := mustRing(t, rotationOldKeyHex)
	ring := mustRing(t, rotationNewKeyHex, rotationOldKeyHex)
	f := seedRotationFixture(t, ctx, client, old, ring, true)
	require.NoError(t, StoreEncryptionKeyFingerprint(ctx, client, old.PrimaryFingerprint()))
	_, err := client.ChannelMonitor.Create().SetName("garbage").SetProvider(channelmonitor.ProviderOpenai).SetAPIMode(service.MonitorAPIModeResponses).
		SetEndpoint("https://api.example.com").SetAPIKeyEncrypted("not-a-ciphertext").SetPrimaryModel("gpt-5").SetIntervalSeconds(60).SetCreatedBy(1).Save(ctx)
	require.NoError(t, err)

	report, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
	require.NoError(t, err)
	require.True(t, report.Failed())
	require.False(t, report.FingerprintUpdated)

	users := storeReport(t, report, "users.")
	require.Equal(t, 1, users.Failed)
	require.Equal(t, 2, users.Rotated, "other rows are still rotated")
	require.Len(t, users.Failures, 1)
	require.Equal(t, fmt.Sprintf("users[%d]", f.lostUser), users.Failures[0].Row)
	require.Contains(t, users.Failures[0].Err, "not configured", "error names the missing key")
	monitors := storeReport(t, report, "channel_monitors.")
	require.Equal(t, 1, monitors.Failed)
	require.Contains(t, monitors.Failures[0].Err, "legacy format")

	stored, _, err := ReadEncryptionKeyFingerprint(ctx, client)
	require.NoError(t, err)
	require.Equal(t, old.PrimaryFingerprint(), stored, "fingerprint must keep pointing at the old key while rows remain undecryptable")

	var buf strings.Builder
	report.Write(&buf)
	require.Contains(t, buf.String(), fmt.Sprintf("failed: users[%d]:", f.lostUser))
	require.Contains(t, buf.String(), "stored fingerprint NOT updated")
}

func TestRotateEncryptionKey_NoFingerprintYetIsReported(t *testing.T) {
	ctx := context.Background()
	client := newRotationSQLiteClient(t)
	ring := mustRing(t, rotationNewKeyHex, rotationOldKeyHex)
	report, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
	require.NoError(t, err)
	require.Empty(t, report.StoredFingerprintKeyID)
	require.True(t, report.FingerprintUpdated, "an empty database is a successful rotation")
	var buf strings.Builder
	report.Write(&buf)
	require.Contains(t, buf.String(), "stored_fingerprint=none")

	_, err = RotateEncryptionKey(ctx, nil, ring, RotateOptions{})
	require.Error(t, err)
	_, err = RotateEncryptionKey(ctx, client, nil, RotateOptions{})
	require.Error(t, err)
}

func TestRewriteJSONStrings(t *testing.T) {
	// "legacy" 代表历史明文：被收编（加密）而不是失败，走 RotateAdoptedLegacyPlaintext。
	upper := func(s string) (string, secretcipher.RotateOutcome, error) {
		switch {
		case s == "boom":
			return "", secretcipher.RotateUnchanged, fmt.Errorf("cannot rotate %q", s)
		case s == "legacy":
			return "LEGACY", secretcipher.RotateAdoptedLegacyPlaintext, nil
		case strings.ToUpper(s) != s:
			return strings.ToUpper(s), secretcipher.RotateReEncrypted, nil
		default:
			return s, secretcipher.RotateUnchanged, nil
		}
	}
	tests := []struct {
		name                  string
		raw                   string
		path                  []string
		want                  string
		changed, saw, adopted bool
		wantErr               string
	}{
		{name: "top_level_string", raw: `{"a":"x","n":9007199254740993}`, path: []string{"a"}, want: `{"a":"X","n":9007199254740993}`, changed: true, saw: true},
		{name: "already_current", raw: `{"a":"X"}`, path: []string{"a"}, want: `{"a":"X"}`, saw: true},
		{name: "missing_key", raw: `{"b":1}`, path: []string{"a"}, want: `{"b":1}`},
		{name: "null_key", raw: `{"a":null}`, path: []string{"a"}, want: `{"a":null}`},
		{name: "empty_string", raw: `{"a":""}`, path: []string{"a"}, want: `{"a":""}`},
		{name: "array_elements", raw: `{"eps":[{"t":"x"},{"t":"Y"},{"o":1}]}`, path: []string{"eps", "[]", "t"}, want: `{"eps":[{"t":"X"},{"t":"Y"},{"o":1}]}`, changed: true, saw: true},
		{name: "array_untouched", raw: `{"eps":[{"t":"Y"}]}`, path: []string{"eps", "[]", "t"}, want: `{"eps":[{"t":"Y"}]}`, saw: true},
		{name: "not_a_string", raw: `{"a":1}`, path: []string{"a"}, wantErr: "expected a JSON string"},
		{name: "not_an_object", raw: `[1]`, path: []string{"a"}, wantErr: "expected a JSON object"},
		{name: "not_an_array", raw: `{"eps":{}}`, path: []string{"eps", "[]", "t"}, wantErr: "expected a JSON array"},
		{name: "leaf_error_is_located", raw: `{"eps":[{"t":"ok"},{"t":"boom"}]}`, path: []string{"eps", "[]", "t"}, wantErr: "eps: [1]: t: cannot rotate"},
		{name: "legacy_plaintext_is_adopted", raw: `{"a":"legacy"}`, path: []string{"a"}, want: `{"a":"LEGACY"}`, changed: true, saw: true, adopted: true},
		{name: "legacy_plaintext_in_array", raw: `{"eps":[{"t":"Y"},{"t":"legacy"}]}`, path: []string{"eps", "[]", "t"}, want: `{"eps":[{"t":"Y"},{"t":"LEGACY"}]}`, changed: true, saw: true, adopted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed, saw, adopted, err := rewriteJSONStrings([]byte(tt.raw), tt.path, upper)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, string(out))
			require.Equal(t, tt.changed, changed)
			require.Equal(t, tt.saw, saw)
			require.Equal(t, tt.adopted, adopted, "历史明文收编必须能被上层单独计数")
		})
	}
}
