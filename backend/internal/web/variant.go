package web

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// distDirName 是 //go:embed all:dist 嵌入的根目录名，其下每个子目录是一套前端产物：
	// dist/default 是基础版（frontend/），dist/<name> 是 frontend-variants/<name> 的构建结果。
	distDirName = "dist"
	// indexHTMLName 是判定"这个目录是一套可服务的前端"的凭据：没有它就没法响应 SPA 路由。
	indexHTMLName = "index.html"
)

// ErrVariantNotFound 表示选中的前端变体在这个二进制里不存在。
// 调用方必须让启动失败，不许回落到 default——回落会把"配错了"伪装成"没生效"，
// 运维看到的是一个功能正常但界面不对的服务，没有任何线索指向环境变量。
var ErrVariantNotFound = errors.New("frontend variant not found")

// unknownVariantError 统一拼装"选不中"的错误：一定带上这个二进制里真正可用的变体名，
// 让运维不用去翻镜像就知道该填什么。
func unknownVariantError(name string, available []string, cause error) error {
	list := "(none)"
	if len(available) > 0 {
		list = strings.Join(available, ", ")
	}
	if cause != nil {
		return fmt.Errorf("%w: %v; available variants: %s", ErrVariantNotFound, cause, list)
	}
	return fmt.Errorf("%w: %q; available variants: %s", ErrVariantNotFound, name, list)
}
