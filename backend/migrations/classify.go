package migrations

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind 是一个迁移对「滚动更新」的影响等级，由低到高排序。
//
// 滚动更新期间新旧两个版本同时在线：新版本启动时先跑迁移，旧副本此时仍在用旧 schema
// 读写。判定规则用一句话说就是：**Up 段删除、重命名、改类型、清空了旧副本仍可能引用的
// 对象或数据，就是 destructive；只是改写已有表里的行，是 data-rewrite；其余都是 additive。**
type Kind string

const (
	// KindAdditive 只新增对象（表、列、索引、函数、触发器、约束）或删除查询永远不会点名的
	// 对象（索引、触发器）。旧副本照常工作，可以滚动更新。
	KindAdditive Kind = "additive"
	// KindDataRewrite 不改 schema，但 INSERT/UPDATE/DELETE 了迁移之前就存在的表。
	// 滚动更新在 schema 上是安全的，但旧副本可能在迁移提交前后读到不同的数据，需要知情。
	KindDataRewrite Kind = "data-rewrite"
	// KindDestructive 旧副本的查询或写入会硬失败，或者数据无法回退。必须先缩到零副本、
	// 先备份，再升级。
	KindDestructive Kind = "destructive"
)

// severity 供 max 合并：一个文件的等级是其中最严重语句的等级。
func (k Kind) severity() int {
	switch k {
	case KindDestructive:
		return 2
	case KindDataRewrite:
		return 1
	default:
		return 0
	}
}

// 分类器独有的语句形态；与 schema_effects.go 共享的 CREATE/DROP/RENAME 识别定义在那里。
// 所有匹配都在 normalizeSQLText 之后进行：注释已去掉，CREATE FUNCTION 的函数体已置空
// （迁移时不执行），DO 块体保留（迁移时执行）。
var (
	reAddConstraint   = regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(;,]+)`)
	reAlterTable      = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([^\s;,(]+)`)
	reAlterColumnType = regexp.MustCompile(`(?i)\bALTER\s+(?:COLUMN\s+)?([^\s;,]+)\s+(?:SET\s+DATA\s+)?TYPE\b`)
	reTruncate        = regexp.MustCompile(`(?i)\bTRUNCATE\s+(?:TABLE\s+)?(?:ONLY\s+)?([^\s;,]+)`)
	reDelete          = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+(?:ONLY\s+)?([^\s;,]+)`)
	reWhere           = regexp.MustCompile(`(?i)\bWHERE\b`)
	reUpdate          = regexp.MustCompile(`(?i)\bUPDATE\s+(?:ONLY\s+)?([^\s;,]+)(?:\s+(?:AS\s+)?[A-Za-z_]\w*)?\s+SET\b`)
	reInsert          = regexp.MustCompile(`(?i)\b(?:INSERT|MERGE)\s+INTO\s+([^\s;,(]+)`)
)

// DROP 后面跟这些词时，删除的是查询永远不会点名的东西（索引、触发器、策略……）或者是
// 列属性的放宽（DROP NOT NULL / DROP DEFAULT），对正在运行的旧副本没有影响。
var harmlessDropKinds = map[string]bool{
	"index": true, "trigger": true, "policy": true, "rule": true, "statistics": true,
	"not": true, "default": true, "identity": true, "expression": true,
}

// 这些词出现在 UPDATE/INSERT 之后时不是表名（ON CONFLICT DO UPDATE SET、FOR UPDATE SKIP LOCKED）。
var notTableTokens = map[string]bool{
	"set": true, "of": true, "on": true, "skip": true, "nowait": true,
}

// createdObjects 记录同一个 Up 段自己创建的对象。
// 删除自己刚建的东西（临时辅助函数、约束替换时的 DROP+ADD 同名）不是破坏：
// 旧副本从未见过这些对象，也就不可能依赖它们。
type createdObjects struct {
	byKind      map[string]map[string]bool
	constraints map[string]bool
}

func collectCreatedObjects(statements []string) *createdObjects {
	created := &createdObjects{byKind: map[string]map[string]bool{}, constraints: map[string]bool{}}
	for _, stmt := range statements {
		for _, m := range reCreateObject.FindAllStringSubmatch(stmt, -1) {
			created.add(normalizeKind(m[1]), normalizeIdentifier(m[2]))
		}
		for _, m := range reAddConstraint.FindAllStringSubmatch(stmt, -1) {
			created.constraints[normalizeIdentifier(m[1])] = true
		}
	}
	return created
}

func (c *createdObjects) add(kind, name string) {
	if c.byKind[kind] == nil {
		c.byKind[kind] = map[string]bool{}
	}
	c.byKind[kind][name] = true
}

func (c *createdObjects) has(kind, name string) bool {
	return c.byKind[kind][name]
}

func (c *createdObjects) hasAnyKind(name string) bool {
	for _, names := range c.byKind {
		if names[name] {
			return true
		}
	}
	return false
}

type classification struct {
	kind    Kind
	reasons []string
	seen    map[string]bool
}

func (c *classification) add(kind Kind, format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	if c.seen[reason] {
		return
	}
	c.seen[reason] = true
	c.reasons = append(c.reasons, reason)
	if kind.severity() > c.kind.severity() {
		c.kind = kind
	}
}

// Classify 只看迁移文件的 Up 段（ExecutableSQL），给出滚动更新影响等级与逐条理由。
//
// 已知的检测边界（有意为之，写在这里而不是藏在实现里）：
//   - 只识别静态可见的对象名；EXECUTE format('... %I ...') 这类动态名字会按出现的字面
//     形态报告，方向是宁可误报。
//   - 不识别「收紧」类变更（SET NOT NULL、ADD CONSTRAINT 收窄取值、无默认值的 NOT NULL 新列）。
//     它们只影响省略该列或写 NULL 的旧写入方；本仓库的 ent 与原生 SQL 都显式列出写入列，
//     历史迁移里也没有这类语句。
//   - 单引号字符串内容参与匹配（见 normalizeSQLText 的说明）。
func Classify(content string) (Kind, []string, error) {
	up, err := ExecutableSQL(content)
	if err != nil {
		return "", nil, err
	}
	text := normalizeSQLText(up, sqlTextOptions{blankDefinitionBodies: true})
	statements := SplitStatements(text)
	created := collectCreatedObjects(statements)
	result := &classification{kind: KindAdditive, seen: map[string]bool{}}
	for _, stmt := range statements {
		classifyStatement(stmt, created, result)
	}
	return result.kind, result.reasons, nil
}

func classifyStatement(stmt string, created *createdObjects, result *classification) {
	alterTarget := ""
	if m := reAlterTable.FindStringSubmatch(stmt); m != nil {
		alterTarget = normalizeIdentifier(m[1])
		if created.has("table", alterTarget) {
			// 改的是本迁移自己建的表：无论怎么改，旧副本都没见过它。
			return
		}
	}

	if m := reRename.FindStringSubmatch(stmt); m != nil && !created.hasAnyKind(normalizeIdentifier(m[1])) {
		result.add(KindDestructive, "ALTER %s %s", normalizeIdentifier(m[1]), compactSQL(m[2]))
	}

	for _, loc := range reDrop.FindAllStringSubmatchIndex(stmt, -1) {
		kind := strings.ToLower(stmt[loc[4]:loc[5]])
		if loc[2] >= 0 {
			kind = "materialized " + kind
		}
		rest := stmt[loc[1]:]
		switch {
		case harmlessDropKinds[kind]:
			continue
		case kind == "constraint":
			name := firstDropName(rest)
			if created.constraints[name] {
				continue // DROP + ADD 同名约束是替换，不是删除
			}
			result.add(KindDestructive, "DROP CONSTRAINT %s on %s (no ADD CONSTRAINT with the same name in this migration)", name, alterTarget)
		case kind == "column" || (alterTarget != "" && !isObjectKind(kind)):
			// ALTER TABLE t DROP [COLUMN] [IF EXISTS] c —— COLUMN 关键字可省略，
			// 省略时 reDrop 抓到的“kind”其实就是列名或 IF。
			name := firstDropName(rest)
			if kind != "column" {
				name = firstDropName(stmt[loc[4]:])
			}
			result.add(KindDestructive, "DROP COLUMN %s.%s", alterTarget, name)
		case isObjectKind(kind):
			for _, name := range dropNameList(rest) {
				if created.has(kind, name) {
					continue // 本迁移自己建又自己删（临时辅助函数/表）
				}
				result.add(KindDestructive, "DROP %s %s", strings.ToUpper(kind), name)
			}
		default:
			// 未列举的 DROP 种类（ROLE、OWNED、DATABASE……）一律按破坏处理：
			// 宁可让运维多停一次机，也不放过一个没见过的删除。
			result.add(KindDestructive, "DROP %s %s", strings.ToUpper(kind), firstDropName(rest))
		}
	}

	if alterTarget != "" {
		for _, m := range reAlterColumnType.FindAllStringSubmatch(stmt, -1) {
			result.add(KindDestructive, "ALTER COLUMN %s.%s TYPE", alterTarget, normalizeIdentifier(m[1]))
		}
	}

	for _, m := range reTruncate.FindAllStringSubmatch(stmt, -1) {
		name := normalizeIdentifier(m[1])
		if created.has("table", name) {
			continue
		}
		result.add(KindDestructive, "TRUNCATE %s", name)
	}

	for _, m := range reDelete.FindAllStringSubmatch(stmt, -1) {
		name := normalizeIdentifier(m[1])
		if created.has("table", name) {
			continue
		}
		if !reWhere.MatchString(stmt) {
			result.add(KindDestructive, "DELETE FROM %s without WHERE", name)
			continue
		}
		result.add(KindDataRewrite, "DELETE FROM %s (filtered)", name)
	}

	for _, m := range reUpdate.FindAllStringSubmatch(stmt, -1) {
		name := normalizeIdentifier(m[1])
		if notTableTokens[name] || created.has("table", name) {
			continue
		}
		result.add(KindDataRewrite, "UPDATE %s", name)
	}

	for _, m := range reInsert.FindAllStringSubmatch(stmt, -1) {
		name := normalizeIdentifier(m[1])
		if created.has("table", name) {
			continue
		}
		result.add(KindDataRewrite, "INSERT INTO %s", name)
	}
}

// isObjectKind 判断 DROP 的对象是否是查询可以点名的一类（表、视图、函数、类型……）。
func isObjectKind(kind string) bool {
	switch kind {
	case "table", "view", "materialized view", "function", "procedure", "type", "domain", "sequence", "schema", "extension":
		return true
	}
	return false
}

func compactSQL(s string) string {
	return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
}
