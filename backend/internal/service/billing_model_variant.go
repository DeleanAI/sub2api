package service

import "strings"

// 计费变体：同一个模型按不同计价维度的价格档，计费名写作 "<模型>@<变体>"，例如 Seedance 的
// "doubao-seedance-2-5-260628@720p+video"。变体名只进计费——定价查找（分组 / 渠道定价按它配）
// 与用量行的 model——路由、分组白名单、账号映射看到的永远是模型本身。
//
// 变体与模型本身的关系只在这里声明：BillingVariantModel 组出计费名，billingVariantBase 还原模型本身。
const billingVariantSeparator = "@"

// BillingVariantModel 返回 model 的计费变体名；variant 为空时就是 model 本身。
func BillingVariantModel(model, variant string) string {
	model, variant = strings.TrimSpace(model), strings.TrimSpace(variant)
	if model == "" || variant == "" {
		return model
	}
	return model + billingVariantSeparator + variant
}

// billingVariantBase 还原计费变体对应的模型本身；model 不是变体时返回 ok=false。
func billingVariantBase(model string) (string, bool) {
	model = strings.TrimSpace(model)
	i := strings.LastIndex(model, billingVariantSeparator)
	if i <= 0 || i == len(model)-len(billingVariantSeparator) {
		return "", false
	}
	return model[:i], true
}
