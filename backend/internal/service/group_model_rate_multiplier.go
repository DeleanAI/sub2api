package service

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 分组逐模型倍率（groups.model_rate_multipliers JSONB，migration 229）。
//
// 语义：token 计费的有效倍率 = (用户覆盖 ?? 分组默认) × 高峰因子 × 逐模型因子。
// 逐模型因子只"乘入"，永远不替换基础倍率；按次/图片/视频计费走各自的独立倍率，
// 不受本表影响（BillingModeToken 之外 ModelRateMultiplier 恒为 1）。
//
// 与 groups.model_pricing（绝对价覆盖）刻意分列存储：定价表是"价格"，本表是"倍率"，
// 倍率条目绝不能进入定价解析链（一条只有倍率没有价格的 model_pricing 条目会在
// ModelPricingResolver 里短路成"分组自定价"而把渠道/内置价清零）。

// GroupModelRateMultiplier 是分组逐模型倍率的一条规则：模型模式 → 倍率。
// 列表有序，计费时按顺序取第一条命中的规则（ModelRateMultiplierFor）。
type GroupModelRateMultiplier struct {
	// ModelPattern 支持精确模型名或末尾 * 通配（如 "claude-opus-*"），
	// 匹配规则与分组逐模型定价完全相同（matchModelPatternNormalized）。
	ModelPattern string `json:"model_pattern"`
	// Multiplier 取值范围 (0, MaxGroupModelRateMultiplier]。
	Multiplier float64 `json:"multiplier"`
}

// MaxGroupModelRateMultiplier 是单条逐模型倍率允许的上限；与 rate_multiplier 仅要求 >0
// 不同，逐模型因子是叠乘进最终倍率的，给一个明确上限避免配置错位（如把百分比填成倍率）
// 在不知不觉中把价格放大两个量级。
const MaxGroupModelRateMultiplier = 100.0

// validGroupModelRateMultiplierValue 是"倍率数值是否合法"的唯一判定：有限、>0、≤上限。
// 保存校验与热路径的兜底读取共用，避免两处口径漂移。
func validGroupModelRateMultiplierValue(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= MaxGroupModelRateMultiplier
}

// NormalizeGroupModelRateMultipliers 是逐模型倍率列表的唯一校验与归一化入口，
// CreateGroup 与 UpdateGroup 共用。规则：模式去首尾空白且非空；倍率 ∈ (0, 100]；
// 按归一化后的模式去重（"Claude-Opus-*" 与 "claude-opus-*" 视为同一条）。
// 返回的切片是输入的拷贝（nil 输入返回 nil，空列表返回空切片），顺序保持不变——
// 顺序就是匹配优先级，归一化不得重排。
func NormalizeGroupModelRateMultipliers(entries []GroupModelRateMultiplier) ([]GroupModelRateMultiplier, error) {
	if entries == nil {
		return nil, nil
	}
	out := make([]GroupModelRateMultiplier, 0, len(entries))
	seen := make(map[string]int, len(entries))
	for i, entry := range entries {
		pattern := strings.TrimSpace(entry.ModelPattern)
		if pattern == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_RATE_MULTIPLIER_PATTERN_REQUIRED",
				fmt.Sprintf("model_rate_multipliers[%d]: model_pattern is required", i))
		}
		if !validGroupModelRateMultiplierValue(entry.Multiplier) {
			return nil, infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_RATE_MULTIPLIER_OUT_OF_RANGE",
				fmt.Sprintf("model_rate_multipliers[%d] (%s): multiplier must be > 0 and <= %v, got %v",
					i, pattern, MaxGroupModelRateMultiplier, entry.Multiplier))
		}
		key := normalizeChannelPricingModelName(pattern)
		if prev, dup := seen[key]; dup {
			return nil, infraerrors.New(http.StatusBadRequest, "GROUP_MODEL_RATE_MULTIPLIER_DUPLICATE_PATTERN",
				fmt.Sprintf("model_rate_multipliers[%d] (%s) duplicates entry %d", i, pattern, prev))
		}
		seen[key] = i
		out = append(out, GroupModelRateMultiplier{ModelPattern: pattern, Multiplier: entry.Multiplier})
	}
	return out, nil
}

// ModelRateMultiplierFor 按列表顺序返回第一条命中 model 的规则；没有命中返回 ok=false。
// 顺序即优先级：更具体的模式应排在更宽泛的通配之前（UI 与文档同口径）。
func (g *Group) ModelRateMultiplierFor(model string) (GroupModelRateMultiplier, bool) {
	if g == nil || len(g.ModelRateMultipliers) == 0 || strings.TrimSpace(model) == "" {
		return GroupModelRateMultiplier{}, false
	}
	for _, entry := range g.ModelRateMultipliers {
		if matchModelPatternNormalized(entry.ModelPattern, model) != modelPatternNoMatch {
			return entry, true
		}
	}
	return GroupModelRateMultiplier{}, false
}

// invalidModelRateMultiplierWarnSeen 让"存量脏倍率被降级为 1"的告警每个 (分组, 模式)
// 每进程只打一条：该分支位于每请求的计费热路径，逐请求刷屏会淹没日志。
var invalidModelRateMultiplierWarnSeen sync.Map

// resolveGroupModelRateMultiplier 是逐模型因子的唯一解析点：CalculateCostUnified（扣费）、
// effectiveDownstreamMultiplier（利润门 D 与 /v1/sub2api/billing）都经由本函数取值。
// 未配置、未命中、model 为空 → 1。命中但倍率非法（直改 SQL / 迁移残留绕过了保存校验）
// 时按 1 处理并告警——静默按非法值扣费或静默忽略都不可接受，但热路径不能 fail-closed。
func resolveGroupModelRateMultiplier(group *Group, model string) float64 {
	entry, ok := group.ModelRateMultiplierFor(model)
	if !ok {
		return 1
	}
	if !validGroupModelRateMultiplierValue(entry.Multiplier) {
		key := fmt.Sprintf("%d\x1f%s", group.ID, entry.ModelPattern)
		if _, seen := invalidModelRateMultiplierWarnSeen.LoadOrStore(key, struct{}{}); !seen {
			slog.Warn("group_model_rate_multiplier_invalid_degraded_to_one",
				"group_id", group.ID,
				"model_pattern", entry.ModelPattern,
				"multiplier", entry.Multiplier,
				"model", model,
				"reason", "stored multiplier is not finite/positive/within limit; billing and profit gate treat it as 1")
		}
		return 1
	}
	return entry.Multiplier
}

// effectiveDownstreamMultiplier 是"请求时刻用户实际承担的 token 倍率"的唯一组合公式：
//
//	D = resolvedRate（用户覆盖 ?? 分组默认，调用方经 ResolveUserGroupRateMultiplier 取得）
//	    × billingGroup.PeakMultiplierAt(pricingAt)
//	    × 逐模型因子(model)
//
// 利润门（gateway 与 openai 两条装门路径）和 /v1/sub2api/billing 都必须经本函数计算，
// 不得各自相乘；扣费侧（computePeakAwareMultipliers + CalculateCostUnified）按同一公式分两步
// 施加，TestEffectiveDownstreamMultiplierAgreesWithBilling 钉死两边恒等。
// model 为空表示"未知模型"（如 /billing 不带 ?model=），此时逐模型因子按 1 处理。
func effectiveDownstreamMultiplier(billingGroup *Group, resolvedRate float64, pricingAt time.Time, model string) float64 {
	return resolvedRate * billingGroup.PeakMultiplierAt(pricingAt) * resolveGroupModelRateMultiplier(billingGroup, model)
}

// EffectiveDownstreamMultiplier 是 effectiveDownstreamMultiplier 的导出入口，供 handler 层
// （/v1/sub2api/billing）复用同一公式。
func EffectiveDownstreamMultiplier(billingGroup *Group, resolvedRate float64, pricingAt time.Time, model string) float64 {
	return effectiveDownstreamMultiplier(billingGroup, resolvedRate, pricingAt, model)
}

// ResolveGroupModelRateMultiplier 是 resolveGroupModelRateMultiplier 的导出入口（handler 展示用）。
func ResolveGroupModelRateMultiplier(group *Group, model string) float64 {
	return resolveGroupModelRateMultiplier(group, model)
}
