package service

import (
	"regexp"
	"strings"
)

// 计费变体：同一个模型按不同计价维度的价格档，计费名写作 "<模型>@<变体>"，例如 Seedance 的
// "doubao-seedance-2-5-260628@720p+video"。变体名只进计费——定价查找（分组 / 渠道定价按它配）
// 与用量行的 model——路由、分组白名单、账号映射、用量行的请求模型看到的永远是模型本身。
//
// 变体与模型本身的关系只在这里声明：BillingVariantModel 组出计费名，billingVariantBase 还原模型本身，
// 两者按同一个变体语法（billingVariantPattern）校验。
const billingVariantSeparator = "@"

// billingVariantPattern 是变体后缀的唯一语法：<分辨率>[+video]，如 "720p"、"1080p+video"。
// "@" 本身也出现在真实模型名里（Vertex 上的 Claude 写作 "claude-sonnet-4@20250514"），见 "@" 就当变体
// 会让这类模型悄悄套用别的模型的倍率——所以只有符合这个语法的后缀才算变体。
var billingVariantPattern = regexp.MustCompile(`^[0-9]+p(\+video)?$`)

// BillingVariantModel 返回 model 的计费变体名；variant 为空或不符合变体语法时返回 model 本身。
func BillingVariantModel(model, variant string) string {
	model, variant = strings.TrimSpace(model), strings.TrimSpace(variant)
	if model == "" || !billingVariantPattern.MatchString(variant) {
		return model
	}
	return model + billingVariantSeparator + variant
}

// billingVariantBase 还原计费变体对应的模型本身；model 不是变体（没有 "@"，或 "@" 之后不符合变体语法）
// 时返回 ok=false。
func billingVariantBase(model string) (string, bool) {
	model = strings.TrimSpace(model)
	i := strings.LastIndex(model, billingVariantSeparator)
	if i <= 0 || !billingVariantPattern.MatchString(model[i+len(billingVariantSeparator):]) {
		return "", false
	}
	return model[:i], true
}
