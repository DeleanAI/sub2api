// Package frontendvariant 只定义前端变体（frontend-variants/<name>）的名字规则。
//
// 为什么单独一个包：config 要在启动时校验 server.frontend_variant，web 要在嵌入的
// dist/<name> 里查目录，构建脚本要校验目录名。三处必须用同一条规则，否则会出现
// "配置校验通过但运行时找不到目录"这种自相矛盾的失败。这个包不依赖任何其它内部包，
// 所以 config 和 web 都能引它而不产生依赖环。
//
// 构建侧的同一条规则写在 frontend/scripts/build-variant.mjs 的 VARIANT_NAME_PATTERN；
// 两边由 frontend/scripts/variant-name.samples.json 这张共享样本表各测一遍，
// 谁改了自己那一份而没改另一份，样本表会当场把它挡下来。
package frontendvariant

import (
	"errors"
	"fmt"
	"regexp"
)

const (
	// Default 是 server.frontend_variant 未配置时的取值，对应基础版前端（frontend/），
	// 构建产物落在 backend/internal/web/dist/default。
	Default = "default"

	// NamePattern 是变体名的唯一规则：小写字母或数字开头，其后可含小写字母、数字、连字符。
	// 名字既是磁盘目录名又是 embed 路径的一段，所以不接受大写（大小写不敏感文件系统上会撞车）、
	// 下划线以外的分隔符、点（. / .. 会跳出目录）和斜杠。
	NamePattern = `^[a-z0-9][a-z0-9-]*$`
)

var namePattern = regexp.MustCompile(NamePattern)

// ErrEmptyName 表示变体名为空——通常是环境变量写成了 SERVER_FRONTEND_VARIANT= 而不是没写。
var ErrEmptyName = errors.New("frontend variant name is empty")

// ValidateName 校验变体名是否符合 NamePattern。
func ValidateName(name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("frontend variant name %q does not match %s", name, NamePattern)
	}
	return nil
}
