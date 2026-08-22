package migrations

import (
	"errors"
	"fmt"
	"strings"
)

// Sections 是按 goose 注解切分后的迁移文件。
//
// 运行器是前向单向的（forward-only）：只执行 Up 段，Down 段永远不执行。
// 历史上 019/024/037 三个文件带有 goose 注解，而旧运行器把整份文件交给 PostgreSQL，
// 于是 Down 段紧跟着 Up 段一起跑，Up 刚建的表当场被删（issue #1）。
// 这里是唯一解析注解的地方：运行器据此决定执行什么，分类器据此决定检查什么。
type Sections struct {
	// HasMarkers 表示文件含有 goose 注解；为 false 时整份文件就是 Up。
	HasMarkers bool
	// Up 是要执行的 SQL（注解行已去掉，注释保留）。
	Up string
	// Down 仅供检视与测试，运行器永远不会执行它。
	Down string
}

const gooseDirectivePrefix = "+goose"

var (
	errGooseNoUp          = errors.New("goose markers present but no '-- +goose Up' section")
	errGooseEmptyUp       = errors.New("goose Up section contains no executable SQL")
	errGooseSQLBeforeUp   = errors.New("executable SQL before the first goose marker would never run; move it into the Up section")
	errGooseDownBeforeUp  = errors.New("'-- +goose Down' must come after '-- +goose Up'")
	errGooseDuplicateUp   = errors.New("duplicate '-- +goose Up' marker")
	errGooseDuplicateDown = errors.New("duplicate '-- +goose Down' marker")
)

// ParseSections 按 goose 注解切分迁移文件。
//
// 规则（只在这里定义）：
//   - 没有任何注解：整份文件是 Up，原样返回。
//   - 有注解：Up 必须存在且先于 Down；只允许 Up / Down / StatementBegin / StatementEnd，
//     其它指令（如 NO TRANSACTION、ENVSUB）运行器不支持，直接拒绝而不是当注释吞掉。
//   - 第一个注解之前只能是注释：那里的 SQL 不属于任何段，不会被执行，写在那里就是错误。
//   - Up 段必须含有可执行 SQL：一个只有 Down 的文件等于「什么都不做」，应当显式失败。
func ParseSections(content string) (Sections, error) {
	lines := strings.Split(content, "\n")
	var preamble, up, down []string
	section := ""
	seenUp, seenDown := false, false
	for i, line := range lines {
		directive, ok := gooseDirective(line)
		if !ok {
			switch section {
			case "up":
				up = append(up, line)
			case "down":
				down = append(down, line)
			default:
				preamble = append(preamble, line)
			}
			continue
		}
		switch directive {
		case "up":
			if seenUp {
				return Sections{}, fmt.Errorf("line %d: %w", i+1, errGooseDuplicateUp)
			}
			if seenDown {
				return Sections{}, fmt.Errorf("line %d: %w", i+1, errGooseDownBeforeUp)
			}
			seenUp = true
			section = "up"
		case "down":
			if !seenUp {
				return Sections{}, fmt.Errorf("line %d: %w", i+1, errGooseDownBeforeUp)
			}
			if seenDown {
				return Sections{}, fmt.Errorf("line %d: %w", i+1, errGooseDuplicateDown)
			}
			seenDown = true
			section = "down"
		case "statementbegin", "statementend":
			// 仅用于 goose 自己的语句切分；本运行器整段提交给 PostgreSQL，无需切分，
			// 但它们必须出现在某个段内，否则说明文件结构已经错了。
			if section == "" {
				return Sections{}, fmt.Errorf("line %d: '-- +goose %s' outside of an Up/Down section", i+1, directive)
			}
		default:
			return Sections{}, fmt.Errorf("line %d: unsupported goose directive %q (only Up/Down/StatementBegin/StatementEnd are recognized; use the _notx.sql suffix for non-transactional migrations)", i+1, directive)
		}
	}

	if !seenUp && !seenDown {
		return Sections{HasMarkers: false, Up: content}, nil
	}
	if !seenUp {
		return Sections{}, errGooseNoUp
	}
	if strings.TrimSpace(StripComments(strings.Join(preamble, "\n"))) != "" {
		return Sections{}, errGooseSQLBeforeUp
	}
	upText := strings.TrimSpace(strings.Join(up, "\n"))
	if strings.TrimSpace(StripComments(upText)) == "" {
		return Sections{}, errGooseEmptyUp
	}
	return Sections{
		HasMarkers: true,
		Up:         upText,
		Down:       strings.TrimSpace(strings.Join(down, "\n")),
	}, nil
}

// ExecutableSQL 返回迁移文件中应当被运行器执行的部分：
// 无 goose 注解时是整份文件，有注解时只有 Up 段。
func ExecutableSQL(content string) (string, error) {
	sections, err := ParseSections(content)
	if err != nil {
		return "", err
	}
	return sections.Up, nil
}

// gooseDirective 识别形如 `-- +goose Up` 的注解行，返回小写、空白归一化后的指令名。
func gooseDirective(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "--") {
		return "", false
	}
	rest := strings.TrimSpace(trimmed[2:])
	// 前缀大小写不敏感：`-- +GOOSE Down` 若被当成普通注释，它后面的 DROP 就会混进 Up 段执行，
	// 这正是要堵的洞，所以宁可多认一种写法。
	if !strings.HasPrefix(strings.ToLower(rest), gooseDirectivePrefix) {
		return "", false
	}
	directive := strings.TrimSpace(rest[len(gooseDirectivePrefix):])
	return strings.ToLower(strings.Join(strings.Fields(directive), " ")), true
}
