//go:build integration

package repository

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	entmigrate "github.com/Wei-Shaw/sub2api/ent/migrate"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

// tableExpectation 记录一个表名以及它是在哪里被声明或引用的，失败信息据此定位。
type tableExpectation struct {
	table  string
	source string
}

// TestMigrations_EveryDeclaredOrReferencedTableExists 是 issue #1 那一类 bug 的护栏：
// 迁移跑完之后，代码会去读写的每一张表都必须真的存在。期望集合来自三份声明，而不是点名：
//  1. ent schema（ent/migrate.Tables）；
//  2. 迁移文件 Up 段静态可见的 CREATE TABLE，减去后续迁移 DROP/RENAME 掉的；
//  3. internal/ 下所有非测试 Go 源码里原生 SQL 字符串点名的表。
//
// 新加的表、新写的原生 SQL 当天就被覆盖；旧运行器那种「Up 刚建的表被 Down 删掉」在这里会
// 以 ops_alert_silences 缺失的形式暴露。
func TestMigrations_EveryDeclaredOrReferencedTableExists(t *testing.T) {
	ctx := context.Background()
	existing := queryPublicRelations(t, ctx)
	require.NotEmpty(t, existing)

	var expectations []tableExpectation
	expectations = append(expectations, entSchemaTableExpectations()...)
	expectations = append(expectations, migrationTableExpectations(t)...)
	refs := rawSQLTableExpectations(t)
	expectations = append(expectations, refs...)

	// 自检：抽取器必须能看见本包运行器对 schema_migrations 的引用；看不见说明抽取器坏了，
	// 而不是代码里没有原生 SQL。
	require.True(t, containsTable(refs, "schema_migrations"),
		"raw SQL extractor did not find the runner's own schema_migrations references; the extractor is broken")

	missing := map[string][]string{}
	for _, expectation := range expectations {
		if existing[expectation.table] {
			continue
		}
		missing[expectation.table] = append(missing[expectation.table], expectation.source)
	}
	if len(missing) == 0 {
		return
	}
	var lines []string
	for table, sources := range missing {
		lines = append(lines, fmt.Sprintf("  %s\n    referenced by: %s", table, strings.Join(uniqueSortedStrings(sources), ", ")))
	}
	sort.Strings(lines)
	t.Fatalf("%d table(s) declared by schema or referenced by code do not exist after applying every migration:\n%s",
		len(missing), strings.Join(lines, "\n"))
}

func queryPublicRelations(t *testing.T, ctx context.Context) map[string]bool {
	t.Helper()
	rows, err := integrationDB.QueryContext(ctx, `
SELECT c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		existing[name] = true
	}
	require.NoError(t, rows.Err())
	return existing
}

func entSchemaTableExpectations() []tableExpectation {
	out := make([]tableExpectation, 0, len(entmigrate.Tables))
	for _, table := range entmigrate.Tables {
		out = append(out, tableExpectation{table: table.Name, source: "ent/migrate/schema.go"})
	}
	return out
}

// migrationTableExpectations 按执行顺序折叠每个迁移 Up 段对表集合的净效果。
func migrationTableExpectations(t *testing.T) []tableExpectation {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	sort.Strings(names)

	created := map[string]string{} // 表名 → 建表的迁移
	for _, name := range names {
		data, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		effects, err := migrations.TablesAffected(string(data))
		require.NoErrorf(t, err, "%s", name)
		for _, table := range effects.Dynamic {
			t.Logf("%s: skipping dynamically named table %q, it can only be resolved at run time", name, table)
		}
		for _, table := range effects.Dropped {
			delete(created, table)
		}
		for _, table := range effects.Created {
			created[table] = name
		}
	}

	out := make([]tableExpectation, 0, len(created))
	for table, name := range created {
		out = append(out, tableExpectation{table: table, source: "migration " + name})
	}
	return out
}

var (
	// SQL 关键字在本仓库一律大写、表名一律小写 snake_case。只匹配大写关键字，日志和报错里的
	// 英文 “from …”“update …” 就不会被当成 SQL；大写的捕获（SET、LATERAL、ONLY）则一定是关键字。
	reRawSQLTableRef = regexp.MustCompile(`\b(FROM|JOIN|INSERT\s+INTO|UPDATE|DELETE\s+FROM|TRUNCATE(?:\s+TABLE)?)\s+(?:ONLY\s+)?"?([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)"?(\s*\()?`)
	// WITH name [(cols)] AS [[NOT] MATERIALIZED] ( … )：CTE 名不是表
	reRawSQLCTE = regexp.MustCompile(`\b([a-z_][a-z0-9_]*)\s*(?:\([^)]*\))?\s+AS\s+(?:(?:NOT\s+)?MATERIALIZED\s+)?\(`)
	// EXTRACT(EPOCH FROM col)、SUBSTRING(x FROM 1) 这类函数里的 FROM 后面跟的是列
	reRawSQLFunctionFrom = regexp.MustCompile(`\b(?:EXTRACT|SUBSTRING|TRIM|OVERLAY|POSITION)\s*\([^)]*\)`)
	reRawSQLDistinctFrom = regexp.MustCompile(`\bDISTINCT\s+FROM\b`)
	// 同一文件里先用 to_regclass 探测过存在性的表是条件引用，不要求存在
	reRawSQLRegclass = regexp.MustCompile(`to_regclass\(\s*'(?:public\.)?([A-Za-z_][A-Za-z0-9_]*)'\s*\)`)
)

// rawSQLTableExpectations 解析 backend/internal 下全部非测试 Go 源码，从字符串字面量里抽出
// 原生 SQL 点名的表。服务层、setup、securityaudit 也有原生 SQL，所以范围是整个 internal/。
func rawSQLTableExpectations(t *testing.T) []tableExpectation {
	t.Helper()
	root := filepath.Join("..", "..", "internal")
	fset := token.NewFileSet()
	var out []tableExpectation
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		var literals []*ast.BasicLit
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				literals = append(literals, lit)
			}
			return true
		})
		// SQL 经常由多段字面量拼接（CTE 定义在一段，FROM 引用在另一段），所以 CTE 名与
		// to_regclass 守卫都按整个文件收集，而不是按单个字面量。
		guarded := map[string]bool{}
		ctes := map[string]bool{}
		texts := make([]string, len(literals))
		for i, lit := range literals {
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return fmt.Errorf("unquote literal at %s: %w", fset.Position(lit.Pos()), err)
			}
			texts[i] = text
			for _, m := range reRawSQLRegclass.FindAllStringSubmatch(text, -1) {
				guarded[m[1]] = true
			}
			for _, m := range reRawSQLCTE.FindAllStringSubmatch(text, -1) {
				ctes[m[1]] = true
			}
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for i, lit := range literals {
			for _, table := range rawSQLTablesIn(texts[i], ctes) {
				if guarded[table] {
					continue
				}
				out = append(out, tableExpectation{
					table:  table,
					source: fmt.Sprintf("internal/%s:%d", filepath.ToSlash(relPath), fset.Position(lit.Pos()).Line),
				})
			}
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// rawSQLTablesIn 从一段文本里抽出被 FROM/JOIN/INSERT INTO/UPDATE/DELETE FROM/TRUNCATE 点名的表。
// 能用机械规则解释掉的标识符（关键字、函数调用、系统目录、alias.column、同文件里的 CTE）都不算。
func rawSQLTablesIn(text string, ctes map[string]bool) []string {
	cleaned := reRawSQLFunctionFrom.ReplaceAllString(text, " ")
	cleaned = reRawSQLDistinctFrom.ReplaceAllString(cleaned, "DISTINCT_FROM")

	var tables []string
	for _, m := range reRawSQLTableRef.FindAllStringSubmatch(cleaned, -1) {
		keyword, ident, paren := m[1], m[2], m[3]
		if paren != "" && (keyword == "FROM" || keyword == "JOIN") {
			continue // unnest(...)、generate_series(...) 这类函数调用
		}
		if strings.ToUpper(ident) == ident {
			continue // 大写：关键字（SET、LATERAL、ONLY、SKIP……）
		}
		if dot := strings.IndexByte(ident, '.'); dot >= 0 {
			if ident[:dot] != "public" {
				continue // pg_catalog.x / information_schema.x 是系统目录，alias.column 是列引用
			}
			ident = ident[dot+1:]
		}
		if strings.HasPrefix(ident, "pg_") {
			continue // 系统目录
		}
		if ctes[ident] {
			continue
		}
		tables = append(tables, ident)
	}
	return tables
}

func containsTable(expectations []tableExpectation, table string) bool {
	for _, expectation := range expectations {
		if expectation.table == table {
			return true
		}
	}
	return false
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
