package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// usageBalanceCurrencyRepo 只回答 balance_currency。
type usageBalanceCurrencyRepo struct {
	service.SettingRepository
	code string
}

func (r *usageBalanceCurrencyRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyBalanceCurrency && r.code != "" {
		return r.code, nil
	}
	return "", service.ErrSettingNotFound
}

type usageBalanceUserRepo struct {
	service.UserRepository
}

func (usageBalanceUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return &service.User{ID: 7, Balance: 20}, nil
}

func (usageBalanceUserRepo) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

// /v1/usage 的三种返回形状（额度限制 key、订阅分组、钱包余额）都用站内余额单位的币种代码作 unit；
// 未配置时仍是 USD（旧客户端看到的值不变）。
func TestUsageUnitIsTheSiteBalanceCurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	shapes := map[string]func(h *GatewayHandler, c *gin.Context){
		"quota_limited": func(h *GatewayHandler, c *gin.Context) {
			h.usageQuotaLimited(c, context.Background(), &service.APIKey{Quota: 10, QuotaUsed: 2, Status: service.StatusAPIKeyActive}, nil, nil, nil)
		},
		"subscription": func(h *GatewayHandler, c *gin.Context) {
			h.usageUnrestricted(c, context.Background(), &service.APIKey{Group: &service.Group{SubscriptionType: service.SubscriptionTypeSubscription}},
				middleware.AuthSubject{}, nil, nil, nil)
		},
		"balance": func(h *GatewayHandler, c *gin.Context) {
			h.usageUnrestricted(c, context.Background(), &service.APIKey{}, middleware.AuthSubject{UserID: 7}, nil, nil, nil)
		},
	}
	for _, site := range []struct{ configured, want string }{{"", "USD"}, {"CNY", "CNY"}} {
		for name, respond := range shapes {
			h := &GatewayHandler{
				settingService: service.NewSettingService(&usageBalanceCurrencyRepo{code: site.configured}, nil),
				userService:    service.NewUserService(usageBalanceUserRepo{}, nil, nil, nil),
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
			respond(h, c)

			require.Equal(t, http.StatusOK, recorder.Code, name)
			var body struct {
				Unit  string `json:"unit"`
				Quota *struct {
					Unit string `json:"unit"`
				} `json:"quota"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), name)
			require.Equal(t, site.want, body.Unit, name)
			if body.Quota != nil {
				require.Equal(t, site.want, body.Quota.Unit, name)
			}
		}
	}
}
