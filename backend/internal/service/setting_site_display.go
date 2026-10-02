package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/text/currency"
)

// DefaultBalanceCurrency 是站内余额单位的默认币种。
//
// 站内的余额、价格、用量金额、限额都是同一个单位。balance_currency 声明这个单位按哪种货币计
// （ISO 4217 代码，例如按「1 单位 = 1 元」运营的站点设为 "CNY"），只决定它怎么写给人和客户端看
// ——界面与邮件里的符号、/v1/usage 的 unit——不做任何换算。支付单的币种是另一回事，见支付配置。
const DefaultBalanceCurrency = "USD"

// BalanceCurrency 是站内余额单位的两种写法：Code 给客户端（/v1/usage 的 unit），
// Symbol 给界面与邮件（写在金额前面）。
type BalanceCurrency struct {
	Code   string
	Symbol string
}

// NormalizeBalanceCurrency 是 balance_currency 的唯一校验与归一化入口（设置写入路径调用）：
// 去首尾空白、转成大写代码；空值回到默认。记账币种决定一切折算（美元官方价、人民币价卡、各币种充值），
// 所以只认有汇率来源的法币（fxCurrencies 里的 fiat），否则每笔计费都会因折算不了而按无价处理。
func NormalizeBalanceCurrency(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultBalanceCurrency, nil
	}
	code, err := NormalizeFiatCurrency(value)
	if err != nil {
		return "", infraerrors.BadRequest("INVALID_BALANCE_CURRENCY",
			fmt.Sprintf("balance_currency must be one of %s (currencies with an exchange rate source), got %q",
				strings.Join(SupportedFXCurrencies(true), ", "), value))
	}
	return code, nil
}

// Format 把站内金额写成给人看的样子：符号 + 两位小数，负数的负号写在符号前（"-¥1.00"）。
// 与前端 utils/balanceCurrency.ts 的 withBalanceCurrencySymbol 同一写法。
func (c BalanceCurrency) Format(amount float64) string {
	if amount < 0 {
		return "-" + c.Symbol + strconv.FormatFloat(-amount, 'f', 2, 64)
	}
	return c.Symbol + strconv.FormatFloat(amount, 'f', 2, 64)
}

// balanceCurrencyOf 由合法代码推导两种写法：符号取 CLDR 的窄符号（USD→"$"、CNY→"¥"、EUR→"€"），
// 没有符号的代码写成代码本身。
func balanceCurrencyOf(code string) BalanceCurrency {
	unit := currency.MustParseISO(code)
	return BalanceCurrency{Code: unit.String(), Symbol: fmt.Sprint(currency.NarrowSymbol(unit))}
}

// balanceCurrencyFromStored 是读路径：库里的值不合法（绕过写入校验直改的）时按默认币种显示并告警，
// 不让它进界面、邮件和客户端。
func balanceCurrencyFromStored(stored string) BalanceCurrency {
	code, err := NormalizeBalanceCurrency(stored)
	if err != nil {
		slog.Warn("setting.balance_currency_invalid_using_default", "stored", stored, "default", DefaultBalanceCurrency, "error", err)
		code = DefaultBalanceCurrency
	}
	return balanceCurrencyOf(code)
}

// ReadBalanceCurrency 是站内余额单位的唯一读取入口，供只持有设置仓库的服务（邮件、批量任务）使用。
// 读设置出错时按默认币种处理并告警：这些调用方都是在发通知或记快照，不值得因此失败。
func ReadBalanceCurrency(ctx context.Context, repo SettingRepository) BalanceCurrency {
	if repo == nil {
		return balanceCurrencyOf(DefaultBalanceCurrency)
	}
	stored, err := repo.GetValue(ctx, SettingKeyBalanceCurrency)
	if err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			slog.Warn("setting.balance_currency_read_failed_using_default", "default", DefaultBalanceCurrency, "error", err)
		}
		return balanceCurrencyOf(DefaultBalanceCurrency)
	}
	return balanceCurrencyFromStored(stored)
}

// BalanceCurrency 返回站内余额单位的两种写法。未装配设置服务（只在测试里出现）时按默认币种。
func (s *SettingService) BalanceCurrency(ctx context.Context) BalanceCurrency {
	if s == nil {
		return ReadBalanceCurrency(ctx, nil)
	}
	return ReadBalanceCurrency(ctx, s.settingRepo)
}

// ErrRedeemDisabled：站点关闭了兑换码（redeem_enabled=false）时，用户侧兑换请求得到它。
var ErrRedeemDisabled = infraerrors.Forbidden("REDEEM_DISABLED", "redeem codes are disabled on this site")

// IsRedeemEnabled 回答用户侧能不能自助兑换兑换码。默认开启（未配置时为开）；管理员发放、
// 支付履约入账不受它影响。读设置出错时按开启处理并告警——兑换本身随后仍要读写同一个库。
func (s *SettingService) IsRedeemEnabled(ctx context.Context) bool {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyRedeemEnabled)
	if err != nil {
		if !errors.Is(err, ErrSettingNotFound) {
			slog.Warn("setting.redeem_enabled_read_failed_assuming_enabled", "error", err)
		}
		return true
	}
	return !isFalseSettingValue(value)
}
