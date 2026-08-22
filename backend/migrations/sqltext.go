package migrations

import (
	"strings"
)

// sqlTextOptions 控制 normalizeSQLText 的输出形态。
type sqlTextOptions struct {
	// blankDefinitionBodies 为 true 时，把 CREATE FUNCTION/PROCEDURE 等「定义类」语句的
	// $$ 体替换成空白。函数体在迁移执行时并不运行，只是被存起来；分类器若把函数体里的
	// UPDATE/DELETE 当成迁移本身的写操作，会把纯粹的函数重定义误报成数据改写。
	// DO $$ ... $$ 块相反：它的体在迁移时立刻执行，必须保留并继续扫描。
	blankDefinitionBodies bool
}

// normalizeSQLText 把 SQL 文本整理成可以用正则安全匹配的形态：
//   - 去掉 -- 行注释与 /* */ 块注释（支持嵌套），用空格占位以保持 token 间距
//   - 单引号字符串、双引号标识符原样保留（包括内容），” 与 E'\” 转义不会提前结束字符串
//   - 美元引号（$$ / $tag$）按所属语句处理：DO 块体递归整理后保留，其它体按 opts 决定
//
// 字符串字面量内容之所以保留，是因为 EXECUTE 'ALTER TABLE ...' 这类动态 DDL 在本仓库的
// 迁移里真实存在（120a、035），漏掉它们会让破坏性语句逃过分类；代价是 COMMENT ON ... IS
// '...' 里的措辞可能造成误报，而误报只会把一次滚动更新降级成停机更新，方向是安全的。
func normalizeSQLText(sql string, opts sqlTextOptions) string {
	var out strings.Builder
	out.Grow(len(sql))
	n := len(sql)
	stmtStart := 0 // 当前语句在 out 中的起点，用于判断美元引号体属于哪种语句
	i := 0
	for i < n {
		c := sql[i]
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			j := i + 2
			for j < n && sql[j] != '\n' {
				j++
			}
			out.WriteByte(' ')
			i = j
		case c == '/' && i+1 < n && sql[i+1] == '*':
			depth := 1
			j := i + 2
			for j < n && depth > 0 {
				switch {
				case sql[j] == '/' && j+1 < n && sql[j+1] == '*':
					depth++
					j += 2
				case sql[j] == '*' && j+1 < n && sql[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			out.WriteByte(' ')
			i = j
		case c == '\'':
			j := scanSingleQuoted(sql, i)
			out.WriteString(sql[i:j])
			i = j
		case c == '"':
			j := scanDoubleQuoted(sql, i)
			out.WriteString(sql[i:j])
			i = j
		case c == '$':
			tag, ok := dollarQuoteTag(sql, i)
			if !ok {
				out.WriteByte(c)
				i++
				continue
			}
			bodyStart := i + len(tag)
			rel := strings.Index(sql[bodyStart:], tag)
			if rel < 0 {
				// 未闭合的美元引号：剩余全部视为体，原样保留，避免吞掉后文。
				out.WriteString(sql[i:])
				i = n
				continue
			}
			body := sql[bodyStart : bodyStart+rel]
			out.WriteString(tag)
			if isDoBlock(out.String()[stmtStart:]) {
				out.WriteString(normalizeSQLText(body, opts))
			} else if opts.blankDefinitionBodies {
				out.WriteByte(' ')
			} else {
				out.WriteString(normalizeSQLText(body, opts))
			}
			out.WriteString(tag)
			i = bodyStart + rel + len(tag)
		case c == ';':
			out.WriteByte(c)
			i++
			stmtStart = out.Len()
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// scanSingleQuoted 返回从 start（指向起始单引号）开始的字符串字面量的结束偏移（不含）。
func scanSingleQuoted(sql string, start int) int {
	n := len(sql)
	// E'...' 形式允许反斜杠转义；判断前缀 E 是否独立（不是某个标识符的结尾）。
	backslashEscapes := start > 0 && (sql[start-1] == 'E' || sql[start-1] == 'e') &&
		(start-1 == 0 || !isIdentByte(sql[start-2]))
	j := start + 1
	for j < n {
		switch {
		case backslashEscapes && sql[j] == '\\':
			j += 2
		case sql[j] == '\'':
			if j+1 < n && sql[j+1] == '\'' {
				j += 2
				continue
			}
			return j + 1
		default:
			j++
		}
	}
	return n
}

// scanDoubleQuoted 返回从 start（指向起始双引号）开始的带引号标识符的结束偏移（不含）。
func scanDoubleQuoted(sql string, start int) int {
	n := len(sql)
	j := start + 1
	for j < n {
		if sql[j] == '"' {
			if j+1 < n && sql[j+1] == '"' {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return n
}

// dollarQuoteTag 判断 sql[i] 处是否是美元引号起始（$$ 或 $tag$），是则返回完整 tag。
// $1 这类位置参数不是美元引号：tag 首字符不能是数字。
func dollarQuoteTag(sql string, i int) (string, bool) {
	n := len(sql)
	if i >= n || sql[i] != '$' {
		return "", false
	}
	j := i + 1
	for j < n && isIdentByte(sql[j]) {
		j++
	}
	if j >= n || sql[j] != '$' {
		return "", false
	}
	if j > i+1 && sql[i+1] >= '0' && sql[i+1] <= '9' {
		return "", false
	}
	return sql[i : j+1], true
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isDoBlock 判断语句前缀的首个关键字是否为 DO（匿名代码块，迁移时立即执行）。
func isDoBlock(stmtPrefix string) bool {
	fields := strings.Fields(stmtPrefix)
	return len(fields) > 0 && strings.EqualFold(fields[0], "DO")
}

// StripComments 去掉注释并保留其余文本（包括所有美元引号体）。
// 运行器也用它判断一条语句是否只剩注释，不另写一套。
func StripComments(sql string) string {
	return normalizeSQLText(sql, sqlTextOptions{})
}

// SplitStatements 按分号切分并去掉空白片段。分号在 plpgsql 体内同样是语句结束符，
// 所以 DO 块里的每条内部语句也会成为独立片段，便于按语句做模式匹配；
// 运行器只对 *_notx.sql（仅允许并发索引语句）逐条执行时使用它。
func SplitStatements(sql string) []string {
	parts := strings.Split(sql, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

// normalizeIdentifier 把 SQL 对象名整理成可比较的形式：去引号、去 public. 前缀、小写。
// 未加引号的标识符在 PostgreSQL 里本来就会折叠成小写；带引号保留大小写的情况在本仓库不存在，
// 统一小写换来更稳定的匹配。
func normalizeIdentifier(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimRight(name, ",;)")
	name = strings.ReplaceAll(name, `"`, "")
	name = strings.ToLower(name)
	name = strings.TrimPrefix(name, "public.")
	return name
}
