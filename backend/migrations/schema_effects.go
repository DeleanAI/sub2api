package migrations

import (
	"regexp"
	"strings"
)

// 迁移文件里改变「对象集合」的语句形态。分类器（classify.go）与表级净效果（TablesAffected）
// 共用这一套识别，不各写一遍正则。所有匹配都在 normalizeSQLText 之后进行。
var (
	reCreateObject = regexp.MustCompile(`(?i)\bCREATE\s+(?:OR\s+REPLACE\s+)?(?:(?:UNLOGGED|TEMP|TEMPORARY|GLOBAL|LOCAL|RECURSIVE)\s+)*(TABLE|VIEW|MATERIALIZED\s+VIEW|FUNCTION|PROCEDURE|TYPE|DOMAIN|SEQUENCE|SCHEMA|EXTENSION)\s+(?:IF\s+NOT\s+EXISTS\s+)?([^\s(;,]+)`)
	reDrop         = regexp.MustCompile(`(?i)\bDROP\s+(?:(MATERIALIZED)\s+)?(\w+)`)
	reIfExists     = regexp.MustCompile(`(?i)^\s*IF\s+EXISTS\s+`)
	reDropListEnd  = regexp.MustCompile(`(?i)\b(?:CASCADE|RESTRICT)\b`)
	reParenGroup   = regexp.MustCompile(`\([^)]*\)`)
	reRename       = regexp.MustCompile(`(?i)\bALTER\s+(?:TABLE|VIEW|MATERIALIZED\s+VIEW)\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([^\s;,(]+)\s+((?:RENAME|SET\s+SCHEMA)\b[^;]*)`)
	reWhitespace   = regexp.MustCompile(`\s+`)
)

func normalizeKind(kind string) string {
	return strings.ToLower(reWhitespace.ReplaceAllString(strings.TrimSpace(kind), " "))
}

// firstDropName 取 DROP <kind> 之后的第一个对象名（跳过 IF EXISTS）。
func firstDropName(rest string) string {
	rest = reIfExists.ReplaceAllString(rest, "")
	rest = strings.TrimSpace(rest)
	end := strings.IndexAny(rest, " \t\r\n,;(")
	if end >= 0 {
		rest = rest[:end]
	}
	return normalizeIdentifier(rest)
}

// dropNameList 解析 DROP TABLE a, b CASCADE 这类逗号列表；函数签名的括号会先去掉。
func dropNameList(rest string) []string {
	rest = reIfExists.ReplaceAllString(rest, "")
	if loc := reDropListEnd.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	rest = reParenGroup.ReplaceAllString(rest, "")
	var names []string
	for _, part := range strings.Split(rest, ",") {
		name := firstDropName(part)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// TableEffects 是 Up 段对表集合的静态可见净效果，供「迁移后的库里应当有哪些表」这类校验使用。
// 按语句顺序折叠，最后一次操作决定归属：先建后删（临时表）只出现在 Dropped，先删后建（重建）只出现在 Created。
type TableEffects struct {
	// Created 是执行完这个文件后应当存在的表：CREATE TABLE 建出的，以及重命名的目标名。
	Created []string
	// Dropped 是执行完这个文件后应当不存在的表：DROP TABLE 删掉的，以及重命名的源名。
	Dropped []string
	// Dynamic 是名字里带 format 占位符（%s/%I）的表，只能在运行期确定，静态校验应跳过它们。
	Dynamic []string
}

// TablesAffected 返回 Up 段对表集合的净效果。与 Classify 共用同一套语句识别，不另写一遍正则。
func TablesAffected(content string) (TableEffects, error) {
	up, err := ExecutableSQL(content)
	if err != nil {
		return TableEffects{}, err
	}
	text := normalizeSQLText(up, sqlTextOptions{blankDefinitionBodies: true})

	var effects TableEffects
	finalState := map[string]bool{} // 表名 → 文件执行完后是否存在；按语句顺序折叠，后者覆盖前者
	var order []string
	record := func(name string, exists bool) {
		if strings.Contains(name, "%") {
			effects.Dynamic = append(effects.Dynamic, name)
			return
		}
		if _, seen := finalState[name]; !seen {
			order = append(order, name)
		}
		finalState[name] = exists
	}
	for _, stmt := range SplitStatements(text) {
		for _, m := range reCreateObject.FindAllStringSubmatch(stmt, -1) {
			if normalizeKind(m[1]) == "table" {
				record(normalizeIdentifier(m[2]), true)
			}
		}
		for _, loc := range reDrop.FindAllStringSubmatchIndex(stmt, -1) {
			if loc[2] >= 0 || !strings.EqualFold(stmt[loc[4]:loc[5]], "table") {
				continue
			}
			for _, name := range dropNameList(stmt[loc[1]:]) {
				record(name, false)
			}
		}
		if m := reRename.FindStringSubmatch(stmt); m != nil {
			fields := strings.Fields(m[2])
			// ALTER TABLE a RENAME TO b；RENAME COLUMN / SET SCHEMA 不改变表名集合
			if len(fields) >= 3 && strings.EqualFold(fields[0], "RENAME") && strings.EqualFold(fields[1], "TO") {
				record(normalizeIdentifier(m[1]), false)
				record(normalizeIdentifier(fields[2]), true)
			}
		}
	}
	for _, name := range order {
		if finalState[name] {
			effects.Created = append(effects.Created, name)
		} else {
			effects.Dropped = append(effects.Dropped, name)
		}
	}
	return effects, nil
}
