package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func fixedPlan(plan *repository.MigrationPlan) migrationPlanner {
	return func(context.Context) (*repository.MigrationPlan, error) { return plan, nil }
}

func requireExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var coded *exitCodeError
	require.ErrorAs(t, err, &coded)
	require.Equal(t, want, coded.code)
	require.Empty(t, coded.message, "判定已在报告里，退出码不再重复打印")
}

func TestMigrateCommandIsRegistered(t *testing.T) {
	cmd, ok := subcommands["migrate"]
	require.True(t, ok)
	require.NotEmpty(t, cmd.summary)
}

func TestRunMigrate_UsageErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigrate(nil, &out, &errOut)
	require.Error(t, err)
	require.Contains(t, errOut.String(), "Usage: sub2api migrate")

	errOut.Reset()
	err = runMigrate([]string{"nope"}, &out, &errOut)
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown subcommand "nope"`)
	require.Contains(t, errOut.String(), "plan")
}

func TestRunMigratePlan_UpToDateExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan(nil, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		AppliedCount: 280,
		LastApplied:  "228_channel_pricing_multipliers.sql",
	}))
	require.NoError(t, err)
	require.Contains(t, out.String(), "applied: 280 (last: 228_channel_pricing_multipliers.sql)")
	require.Contains(t, out.String(), "pending: 0")
	require.Contains(t, out.String(), "verdict: up-to-date")
	require.Contains(t, out.String(), "(exit code 0)")
}

func TestRunMigratePlan_RollingSafeExits10(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan(nil, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		AppliedCount: 2,
		LastApplied:  "002_x.sql",
		Pending: []repository.PendingMigration{
			{Filename: "003_add.sql", Kind: migrations.KindAdditive},
			{Filename: "004_seed.sql", Kind: migrations.KindDataRewrite, Reasons: []string{"INSERT INTO settings"}},
		},
	}))
	requireExitCode(t, err, exitMigratePlanRollingSafe)
	text := out.String()
	require.Contains(t, text, "pending: 2")
	require.Contains(t, text, "003_add.sql")
	require.Contains(t, text, "004_seed.sql")
	require.Contains(t, text, "INSERT INTO settings")
	require.Contains(t, text, "verdict: rolling-safe")
	require.Less(t, strings.Index(text, "003_add.sql"), strings.Index(text, "004_seed.sql"), "按执行顺序输出")
}

func TestRunMigratePlan_DestructiveExits20(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan(nil, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		Pending: []repository.PendingMigration{
			{Filename: "003_add.sql", Kind: migrations.KindAdditive},
			{Filename: "005_drop.sql", Kind: migrations.KindDestructive, Reasons: []string{"DROP TABLE legacy", "DROP COLUMN users.wechat"}},
		},
	}))
	requireExitCode(t, err, exitMigratePlanDestructive)
	require.Contains(t, out.String(), "DROP TABLE legacy; DROP COLUMN users.wechat")
	require.Contains(t, out.String(), "verdict: destructive")
	require.Contains(t, out.String(), "fresh database", "没有已应用记录时要说明这是新库")
}

func TestRunMigratePlan_JSONOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan([]string{"--json"}, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		AppliedCount:   3,
		LastApplied:    "900_newer.sql",
		UnknownApplied: []string{"900_newer.sql"},
		Pending: []repository.PendingMigration{
			{Filename: "005_drop.sql", Kind: migrations.KindDestructive, Reasons: []string{"DROP TABLE legacy"}},
		},
	}))
	requireExitCode(t, err, exitMigratePlanDestructive)

	var report migratePlanReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.Equal(t, 3, report.AppliedCount)
	require.Equal(t, "900_newer.sql", report.LastApplied)
	require.Equal(t, []string{"900_newer.sql"}, report.UnknownApplied)
	require.Equal(t, []migratePlanEntry{{Filename: "005_drop.sql", Kind: "destructive", Reasons: []string{"DROP TABLE legacy"}}}, report.Pending)
	require.Equal(t, verdictDestructive, report.Verdict)
	require.Equal(t, exitMigratePlanDestructive, report.ExitCode)
	require.NotNil(t, report.ChecksumMismatches, "JSON 里用空数组而不是 null，脚本好处理")
}

func TestRunMigratePlan_JSONReasonsNeverNull(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan([]string{"-json"}, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		Pending: []repository.PendingMigration{{Filename: "003_add.sql", Kind: migrations.KindAdditive}},
	}))
	requireExitCode(t, err, exitMigratePlanRollingSafe)
	require.Contains(t, out.String(), `"reasons": []`)
}

func TestRunMigratePlan_QuietPrintsNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan([]string{"--quiet"}, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		Pending: []repository.PendingMigration{{Filename: "005_drop.sql", Kind: migrations.KindDestructive, Reasons: []string{"DROP TABLE legacy"}}},
	}))
	requireExitCode(t, err, exitMigratePlanDestructive)
	require.Empty(t, out.String())
	require.Empty(t, errOut.String())
}

func TestRunMigratePlan_ChecksumMismatchIsAHardError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan(nil, &out, &errOut, fixedPlan(&repository.MigrationPlan{
		AppliedCount: 1,
		LastApplied:  "001_init.sql",
		ChecksumMismatches: []repository.MigrationChecksumMismatch{
			{Filename: "001_init.sql", DBChecksum: "aaa", FileChecksum: "bbb"},
		},
		Pending: []repository.PendingMigration{{Filename: "002_add.sql", Kind: migrations.KindAdditive}},
	}))
	require.Error(t, err)
	var coded *exitCodeError
	require.False(t, errors.As(err, &coded), "阻塞不是 10/20 的判定，而是普通错误（退出码 1）")
	require.Contains(t, err.Error(), "checksum")
	require.Contains(t, out.String(), "001_init.sql  db=aaa file=bbb")
	require.Contains(t, out.String(), "verdict: blocked")
}

func TestRunMigratePlan_PlannerErrorPropagates(t *testing.T) {
	var out, errOut bytes.Buffer
	boom := errors.New("connect refused")
	err := runMigratePlan(nil, &out, &errOut, func(context.Context) (*repository.MigrationPlan, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
	require.Empty(t, out.String())
}

func TestRunMigratePlan_FlagErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigratePlan([]string{"--bogus"}, &out, &errOut, fixedPlan(&repository.MigrationPlan{}))
	require.Error(t, err)
	require.Contains(t, errOut.String(), "Exit codes:")

	errOut.Reset()
	err = runMigratePlan([]string{"extra"}, &out, &errOut, fixedPlan(&repository.MigrationPlan{}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected arguments: extra")
}

func TestExitCodeError_Message(t *testing.T) {
	require.Equal(t, "exit code 20", (&exitCodeError{code: 20}).Error())
	require.Equal(t, "boom", (&exitCodeError{code: 1, message: "boom"}).Error())
}

// TestMigratePlanRunsWithoutEncryptionKey 钉死"升级闸门不依赖落库密文密钥"。
//
// plan 是文档里的升级前置检查，而它曾经走 LoadForBootstrap：release 模式下缺
// TOTP_ENCRYPTION_KEY 直接退出 1，而 1 在闸门契约里表示"阻止升级"。最需要这道闸门的
// 恰恰是升级前还没配过密钥的存量实例——闸门对它们永远不可用，运维只能绕过。
// plan 只读 schema_migrations 和内嵌迁移的校验和，一个密文都不碰。
func TestMigratePlanRunsWithoutEncryptionKey(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	t.Setenv("CONFIG_FILE", "")
	t.Setenv("DATA_DIR", "")
	t.Setenv("SERVER_MODE", "release")
	t.Setenv("JWT_SECRET", strings.Repeat("j", 32))
	t.Setenv("TOTP_ENCRYPTION_KEY", "")
	t.Setenv("TOTP_ENCRYPTION_KEY_PREVIOUS", "")

	_, err := config.LoadForSchemaTooling()
	require.NoError(t, err, "闸门用的配置加载不得要求落库密文密钥")

	_, err = config.LoadForBootstrap()
	require.ErrorContains(t, err, "totp.encryption_key is required when server.mode=release",
		"服务进程的加载仍必须要求它——两者的区别正是本测试要钉住的")
}
