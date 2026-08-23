//go:build integration

package repository

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/schema"

	"github.com/stretchr/testify/require"
)

// provider 名单在两个地方各有一份：ent/schema 的 AuthProviderSpec（应用层校验用）
// 和数据库里 5 张表的 CHECK 约束（迁移写死的字符串）。两份分叉的表现是：应用允许
// 写入、数据库拒绝，注册在最后一步 500，而日志里只有一条约束违例。
//
// 这个测试遍历"哪些表带 provider 约束"的声明，逐张比对数据库里的实际取值集合与
// ent 声明是否完全一致——加 provider 时漏改任何一张表都会在这里失败，并指出
// 缺的是哪张表、哪个值。
func TestMigrations_AuthProviderCheckConstraintsMatchDeclaration(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, ApplyMigrations(ctx, integrationDB))

	// 带 provider 取值约束的表 → 列名。新增这类表时在这里加一行。
	constrained := map[string]string{
		"users":                        "signup_source",
		"auth_identities":              "provider_type",
		"auth_identity_channels":       "provider_type",
		"pending_auth_sessions":        "provider_type",
		"user_provider_default_grants": "provider_type",
	}

	declared := schema.AuthProviderTypes()
	sort.Strings(declared)

	for table, column := range constrained {
		t.Run(table, func(t *testing.T) {
			var definition string
			err := integrationDB.QueryRowContext(ctx, `
				SELECT pg_get_constraintdef(c.oid)
				FROM pg_constraint c
				JOIN pg_class t ON t.oid = c.conrelid
				JOIN pg_namespace n ON n.oid = t.relnamespace
				WHERE n.nspname = 'public'
				  AND t.relname = $1
				  AND c.contype = 'c'
				  AND pg_get_constraintdef(c.oid) LIKE '%' || $2 || '%'
				  AND pg_get_constraintdef(c.oid) LIKE '%ANY%'
				LIMIT 1
			`, table, column).Scan(&definition)
			require.NoErrorf(t, err, "%s.%s 上找不到 provider 取值约束（迁移漏了这张表？）", table, column)

			actual := providerValuesFromConstraint(definition)
			require.Equalf(t, declared, actual,
				"%s.%s 的 CHECK 取值与 ent/schema 的 AuthProviderSpec 声明不一致\n约束定义：%s",
				table, column, definition)
		})
	}
}

// providerValuesFromConstraint 从 pg_get_constraintdef 的输出里抽出取值集合。
// PostgreSQL 会把 IN (...) 规范化成 = ANY (ARRAY['a'::character varying, ...])，
// 所以这里按引号取字面量，再去掉类型标注。
func providerValuesFromConstraint(definition string) []string {
	var values []string
	for _, part := range strings.Split(definition, "'") {
		part = strings.TrimSpace(part)
		if part == "" || strings.ContainsAny(part, "(),:=[]") {
			continue
		}
		values = append(values, part)
	}
	sort.Strings(values)
	return values
}

// 迁移里的取值清单是手写字符串，容易出现"某张表多了一个别的表没有的值"。
// 这条断言把 5 张表两两对齐，失败信息直接给出差异。
func TestMigrations_AuthProviderConstraintsAgreeAcrossTables(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, ApplyMigrations(ctx, integrationDB))

	rows, err := integrationDB.QueryContext(ctx, `
		SELECT t.relname, pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = 'public'
		  AND c.contype = 'c'
		  AND c.conname LIKE '%provider_type_check'
	`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	perTable := map[string]string{}
	for rows.Next() {
		var table, definition string
		require.NoError(t, rows.Scan(&table, &definition))
		perTable[table] = strings.Join(providerValuesFromConstraint(definition), ",")
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, perTable)

	var reference, referenceTable string
	for table, values := range perTable {
		if reference == "" {
			reference, referenceTable = values, table
			continue
		}
		require.Equalf(t, reference, values,
			"provider 取值在 %s 与 %s 上不一致：%s vs %s",
			referenceTable, table, reference, values)
	}
	fmt.Printf("provider constraint values (%d tables): %s\n", len(perTable), reference)
}
