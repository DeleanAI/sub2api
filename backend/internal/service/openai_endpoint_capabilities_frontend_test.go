//go:build unit

package service

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// 前端的能力复选框与「是否等价于未配置」的判断用的是 frontend/src/constants/openaiEndpointCapabilities.ts
// 里的两张表；后端以 ConfigurableOpenAIEndpointCapabilities / DefaultOpenAIEndpointCapabilities 为准。
// 两种语言共享不了一份声明，只能由测试强制对齐——Seedance 曾出现「前端能勾、后端批量编辑拒绝」。
func TestFrontendOpenAIEndpointCapabilitiesMatchBackend(t *testing.T) {
	path := filepath.Join("..", "..", "..", "frontend", "src", "constants", "openaiEndpointCapabilities.ts")
	source, err := os.ReadFile(path)
	require.NoError(t, err, "前端常量文件必须存在：%s", path)

	extract := func(name string) []OpenAIEndpointCapability {
		block := regexp.MustCompile(name + `[^=]*=\s*\[([^\]]*)\]`).FindSubmatch(source)
		require.NotNil(t, block, "前端常量 %s 没找到", name)
		var values []OpenAIEndpointCapability
		for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1) {
			values = append(values, OpenAIEndpointCapability(m[1]))
		}
		return values
	}
	require.Equal(t, ConfigurableOpenAIEndpointCapabilities, extract("CONFIGURABLE_OPENAI_ENDPOINT_CAPABILITIES"))
	require.Equal(t, DefaultOpenAIEndpointCapabilities, extract("DEFAULT_OPENAI_ENDPOINT_CAPABILITIES"))
}
