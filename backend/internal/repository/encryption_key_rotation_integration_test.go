//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

var rotationDBSeq uint64

// newRotationPostgresClient 在测试容器里建一个独立数据库并迁移到位：轮换的
// "全部行成功才更新指纹"是全表断言，不能在被其他测试污染的共享库里验证。
func newRotationPostgresClient(t *testing.T) *dbent.Client {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("sub2api_rotate_%d_%d", time.Now().UnixNano(), atomic.AddUint64(&rotationDBSeq, 1))
	_, err := integrationDB.ExecContext(ctx, `CREATE DATABASE "`+name+`"`)
	require.NoError(t, err)

	u, err := url.Parse(integrationDSN)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := openSQLWithRetry(ctx, u.String(), 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() {
		_ = client.Close()
		_, _ = integrationDB.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`"`)
	})
	return client
}

// PostgreSQL 路径与 SQLite 单元测试的差异正是需要真库才能证明的部分：
// SELECT ... FOR UPDATE 行锁、jsonb 列的读写、$n 占位符。
func TestRotateEncryptionKey_Postgres(t *testing.T) {
	ctx := context.Background()
	client := newRotationPostgresClient(t)
	old := mustRing(t, rotationOldKeyHex)
	ring := mustRing(t, rotationNewKeyHex, rotationOldKeyHex)
	f := seedRotationFixture(t, ctx, client, old, ring, false)
	require.NoError(t, StoreEncryptionKeyFingerprint(ctx, client, old.PrimaryFingerprint()))

	report, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
	require.NoError(t, err)
	require.Equal(t, dialect.Postgres, report.Dialect)
	require.True(t, report.RowLocks, "row locks are on for PostgreSQL")
	require.False(t, report.Failed(), "%+v", report.Stores)
	require.True(t, report.FingerprintUpdated)
	rotated, _, _, _ := report.Totals()
	require.Equal(t, 7, rotated)

	// jsonb 列：只有会话值被改写，其余键原样保留，且整数没有经 float64 往返。
	extraRaw, found, err := queryNullString(ctx, client, `SELECT extra::text FROM accounts WHERE id = $1`, f.oldAccount)
	require.NoError(t, err)
	require.True(t, found)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(extraRaw), &extra))
	assertCurrent(t, ring, extra[service.OllamaCloudUsageSessionExtraKey].(string), "cookie-old")
	require.Contains(t, extraRaw, rotationBigInt)
	require.Equal(t, map[string]any{"keep": true}, extra["nested"])

	totp, found, err := queryNullString(ctx, client, `SELECT totp_secret_encrypted FROM users WHERE id = $1`, f.deletedUser)
	require.NoError(t, err)
	require.True(t, found)
	assertCurrent(t, ring, totp, "totp-deleted")

	paymentRaw, found, err := queryNullString(ctx, client, `SELECT config FROM payment_provider_instances WHERE id = $1`, f.legacyPayment)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, strings.HasPrefix(paymentRaw, "{"), "legacy payment ciphertext converted to plaintext JSON: %s", paymentRaw)

	stored, found, err := ReadEncryptionKeyFingerprint(ctx, client)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ring.PrimaryFingerprint(), stored)

	again, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
	require.NoError(t, err)
	rotated, current, _, failed := again.Totals()
	require.Equal(t, 0, rotated)
	require.Equal(t, 0, failed)
	require.Equal(t, 12, current)
}

// 行锁下并发写：轮换事务持锁期间，服务对同一行的更新必须排在它之后，
// 两边的写入都不会丢。
func TestRotateEncryptionKey_PostgresRowLockSerializesConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	client := newRotationPostgresClient(t)
	old := mustRing(t, rotationOldKeyHex)
	ring := mustRing(t, rotationNewKeyHex, rotationOldKeyHex)
	f := seedRotationFixture(t, ctx, client, old, ring, false)

	// 模拟服务：在轮换锁定该账号行时，改写 extra 里的另一个键。
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	_, err = tx.Client().ExecContext(ctx, `SELECT id FROM accounts WHERE id = $1 FOR UPDATE`, f.oldAccount)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := RotateEncryptionKey(ctx, client, ring, RotateOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("rotation finished while the row was locked by another transaction: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	_, err = tx.Client().ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(extra, '{nested}', '{"keep":false}') WHERE id = $1`, f.oldAccount)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.NoError(t, <-done)

	extraRaw, _, err := queryNullString(ctx, client, `SELECT extra::text FROM accounts WHERE id = $1`, f.oldAccount)
	require.NoError(t, err)
	var extra map[string]any
	require.NoError(t, json.Unmarshal([]byte(extraRaw), &extra))
	require.Equal(t, map[string]any{"keep": false}, extra["nested"], "the concurrent write committed before the rotation read the row")
	assertCurrent(t, ring, extra[service.OllamaCloudUsageSessionExtraKey].(string), "cookie-old")
}
