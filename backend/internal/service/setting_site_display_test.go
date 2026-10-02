//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBalanceCurrency(t *testing.T) {
	for in, want := range map[string]string{"": "USD", "  ": "USD", "USD": "USD", " cny ": "CNY"} {
		got, err := NormalizeBalanceCurrency(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	// EUR 是合法 ISO 代码，但没有汇率来源：记账币种只能是能折算的法币；稳定币不能当记账币种。
	for _, bad := range []string{"EUR", "USDT", "RMB", "US", "¥", "$", "CNY1", "人民币"} {
		_, err := NormalizeBalanceCurrency(bad)
		require.Error(t, err, bad)
		require.Equal(t, "INVALID_BALANCE_CURRENCY", infraerrors.Reason(err), bad)
	}
}

// 符号由币种推导（CLDR 窄符号）；金额写法：符号 + 两位小数，负号在符号前。
func TestBalanceCurrencySymbolAndFormat(t *testing.T) {
	require.Equal(t, BalanceCurrency{Code: "USD", Symbol: "$"}, balanceCurrencyOf("USD"))
	require.Equal(t, BalanceCurrency{Code: "CNY", Symbol: "¥"}, balanceCurrencyOf("CNY"))
	require.Equal(t, "€", balanceCurrencyOf("EUR").Symbol)

	cny := balanceCurrencyOf("CNY")
	require.Equal(t, "¥1.50", cny.Format(1.5))
	require.Equal(t, "-¥1.50", cny.Format(-1.5))
	require.Equal(t, "¥0.00", cny.Format(0))
}

// 读取入口：未装配 / 未配置 / 库里被直改成非法值，都按默认币种。
func TestReadBalanceCurrency(t *testing.T) {
	ctx := context.Background()
	repo := func(values map[string]string) SettingRepository {
		return &paymentFulfillmentSettingRepoStub{values: values}
	}
	require.Equal(t, "USD", ReadBalanceCurrency(ctx, nil).Code)
	require.Equal(t, "USD", ReadBalanceCurrency(ctx, repo(map[string]string{})).Code)
	require.Equal(t, BalanceCurrency{Code: "CNY", Symbol: "¥"}, ReadBalanceCurrency(ctx, repo(map[string]string{SettingKeyBalanceCurrency: "CNY"})))
	require.Equal(t, "USD", ReadBalanceCurrency(ctx, repo(map[string]string{SettingKeyBalanceCurrency: "RMB"})).Code)

	var unwired *SettingService
	require.Equal(t, "$", unwired.BalanceCurrency(ctx).Symbol)
}

// 设置写入路径是唯一的校验点：归一化后落库并同步推导符号，非法代码拒绝保存。
func TestSettingsWriteNormalizesBalanceCurrency(t *testing.T) {
	ctx := context.Background()
	svc := NewSettingService(&paymentFulfillmentSettingRepoStub{values: map[string]string{}}, nil)

	settings := &SystemSettings{BalanceCurrency: " cny "}
	updates, err := svc.buildSystemSettingsUpdates(ctx, settings)
	require.NoError(t, err)
	require.Equal(t, "CNY", updates[SettingKeyBalanceCurrency])
	require.Equal(t, "CNY", settings.BalanceCurrency)
	require.Equal(t, "¥", settings.BalanceCurrencySymbol)

	_, err = svc.buildSystemSettingsUpdates(ctx, &SystemSettings{BalanceCurrency: "RMB"})
	require.Equal(t, "INVALID_BALANCE_CURRENCY", infraerrors.Reason(err))
}

// 公开设置下发兑换开关与站内余额单位（代码 + 推导出的符号）；默认开启 / USD / "$"。
func TestPublicSettingsCarryRedeemSwitchAndBalanceCurrency(t *testing.T) {
	ctx := context.Background()
	defaults, err := NewSettingService(&paymentFulfillmentSettingRepoStub{values: map[string]string{}}, nil).GetPublicSettings(ctx)
	require.NoError(t, err)
	require.True(t, defaults.RedeemEnabled)
	require.Equal(t, "USD", defaults.BalanceCurrency)
	require.Equal(t, "$", defaults.BalanceCurrencySymbol)

	configured := NewSettingService(&paymentFulfillmentSettingRepoStub{values: map[string]string{
		SettingKeyRedeemEnabled: "false", SettingKeyBalanceCurrency: "CNY",
	}}, nil)
	got, err := configured.GetPublicSettings(ctx)
	require.NoError(t, err)
	require.False(t, got.RedeemEnabled)
	require.Equal(t, "CNY", got.BalanceCurrency)
	require.Equal(t, "¥", got.BalanceCurrencySymbol)
	require.False(t, configured.IsRedeemEnabled(ctx))

	// 注入页面的那份（首屏就要用，不能等异步接口）也带着它们。
	injected, err := configured.GetPublicSettingsForInjection(ctx)
	require.NoError(t, err)
	raw, err := json.Marshal(injected)
	require.NoError(t, err)
	require.JSONEq(t, `{"redeem_enabled":false,"balance_currency":"CNY","balance_currency_symbol":"¥"}`,
		pickJSONFields(t, raw, "redeem_enabled", "balance_currency", "balance_currency_symbol"))

	tampered, err := NewSettingService(&paymentFulfillmentSettingRepoStub{values: map[string]string{SettingKeyBalanceCurrency: "RMB"}}, nil).GetPublicSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, "USD", tampered.BalanceCurrency)
	require.Equal(t, "$", tampered.BalanceCurrencySymbol)
}

func pickJSONFields(t *testing.T, raw []byte, keys ...string) string {
	t.Helper()
	var all map[string]any
	require.NoError(t, json.Unmarshal(raw, &all))
	picked := map[string]any{}
	for _, k := range keys {
		v, ok := all[k]
		require.True(t, ok, "injection payload lacks %s", k)
		picked[k] = v
	}
	out, err := json.Marshal(picked)
	require.NoError(t, err)
	return string(out)
}

// 模板化邮件：{{currency_symbol}} 取自站点设置，官方模板里的金额都带着它。
func TestNotificationEmailsWriteAmountsInSiteBalanceCurrency(t *testing.T) {
	ctx := context.Background()
	repo := newNotificationEmailMemorySettingRepo()
	repo.values[SettingKeyBalanceCurrency] = "CNY"
	svc := NewNotificationEmailService(repo, nil)

	for _, event := range []string{NotificationEmailEventBalanceLow, NotificationEmailEventBalanceRechargeSuccess, NotificationEmailEventAccountQuotaAlert} {
		for _, locale := range notificationEmailLocales {
			tmpl, err := svc.GetTemplate(ctx, event, locale)
			require.NoError(t, err)
			preview, err := svc.PreviewTemplate(ctx, NotificationEmailPreviewInput{Event: event, Locale: locale, Subject: tmpl.Subject, HTML: tmpl.HTML})
			require.NoError(t, err)
			require.Contains(t, preview.HTML, "¥", "%s/%s", event, locale)
			require.NotContains(t, preview.HTML, "$", "%s/%s", event, locale)
		}
	}

	// 实际发送时站点变量不接受调用方覆盖。
	variables := svc.runtimeVariables(ctx, NotificationEmailEventBalanceLow, "zh", NotificationEmailSendInput{
		RecipientEmail: "u@example.com",
		Variables:      map[string]string{"currency_symbol": "$", "current_balance": "1.00"},
	})
	require.Equal(t, "¥", variables["currency_symbol"])
}

// 护栏：官方邮件模板与内置兜底模板都不写死金额符号。遍历全部事件与语言。
func TestEmailTemplatesHaveNoHardcodedCurrencySymbol(t *testing.T) {
	for event, locales := range notificationEmailOfficialTemplates {
		for locale, tmpl := range locales {
			require.NotContains(t, tmpl.Subject+tmpl.HTML, "$", "official template %s/%s", event, locale)
		}
	}
	require.NotContains(t, balanceLowEmailTemplate, "$")
	require.NotContains(t, quotaAlertEmailTemplate, "$")
}

// 批量生图任务的金额快照记下提交时的站内余额单位。
func TestBatchImageJobRecordsSiteBalanceCurrency(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		repo SettingRepository
		want string
	}{
		{"unconfigured site", nil, "USD"},
		{"CNY site", &paymentFulfillmentSettingRepoStub{values: map[string]string{SettingKeyBalanceCurrency: "CNY"}}, "CNY"},
	} {
		svc, repo, _, _, _ := newTestBatchImagePublicService(true)
		svc.SettingRepo = tc.repo
		got, err := svc.Submit(ctx, testBatchImageOwner(), validBatchImageSubmitRequest(), "")
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, repo.jobs[got.ID].Currency, tc.name)
	}
}

// 关闭兑换只拦用户侧的自助兑换；管理员发放照常入账。
func TestRedeemSwitchOnlyGatesSelfService(t *testing.T) {
	ctx := context.Background()
	disabled := NewSettingService(&paymentFulfillmentSettingRepoStub{values: map[string]string{SettingKeyRedeemEnabled: "false"}}, nil)

	// 用户侧：在碰任何仓库之前就拒绝（仓库全为 nil，碰了就会 panic）。
	_, err := NewRedeemService(nil, nil, nil, nil, nil, nil, nil, nil, disabled).Redeem(ctx, 42, "ANY")
	require.ErrorIs(t, err, ErrRedeemDisabled)

	code := &RedeemCode{ID: 103, Code: "ADMIN-GRANT", Type: RedeemTypeBalance, Value: 5, Status: StatusUnused}
	redeemRepo := &paymentFulfillmentRedeemRepo{
		paymentOrderLifecycleRedeemRepo: paymentOrderLifecycleRedeemRepo{codesByCode: map[string]*RedeemCode{code.Code: code}},
	}
	userRepo := &mockUserRepo{getByIDUser: &User{ID: 42, CreatedAt: time.Now()}}
	userRepo.updateBalanceFn = func(context.Context, int64, float64) error { return nil }
	svc := NewRedeemService(redeemRepo, userRepo, nil, &paymentFulfillmentRedeemCacheStub{}, nil, newPaymentConfigServiceTestClient(t), nil, nil, disabled)
	result, err := svc.RedeemForAdminFulfillment(ctx, 42, code.Code)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, redeemRepo.useCalls, 1)

	// 默认（未配置）开启。
	require.True(t, NewSettingService(&paymentFulfillmentSettingRepoStub{values: map[string]string{}}, nil).IsRedeemEnabled(ctx))
}
