package schema

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// AuthProviderSpec 是一个登录 provider 的形状声明，也是 provider 名单的唯一来源。
//
// 数据库 5 张表的 CHECK 约束、users.signup_source 与 auth_identities.provider_type 的 ent 校验、
// 个人页身份摘要、signup source 默认授予、后端模式放行路径……凡是"按 provider 点名"的声明点，
// 要么直接遍历这里，要么由护栏测试对照这里逐项断言（service 包的 auth_provider_declaration_test、
// repository 包的 migrations_auth_provider_constraints_integration_test）。
// 新增 provider 只在这里加一行，漏掉任何声明点由测试当场报错，而不是等用户点登录才发现。
type AuthProviderSpec struct {
	// Name 是 provider 标识，同时是 signup_source / provider_type 列的取值。
	Name string
	// BindsIdentity 表示该 provider 以 auth_identities 行绑定到账号：个人页可绑定/解绑，
	// 有 /api/v1/auth/oauth/<name>/{bind/start,create-account,bind-login} 路由、合成邮箱域名
	// 与 UserIdentitySummarySet 字段。email 是本地账号本身；github/google 只是邮箱验证渠道
	// （登录后落成 email 身份），都不单独绑定。
	BindsIdentity bool
}

var authProviderSpecs = []AuthProviderSpec{
	{Name: "email"},
	{Name: "linuxdo", BindsIdentity: true},
	{Name: "wechat", BindsIdentity: true},
	{Name: "oidc", BindsIdentity: true},
	{Name: "github"},
	{Name: "google"},
	{Name: "dingtalk", BindsIdentity: true},
	{Name: "feishu", BindsIdentity: true},
}

// AuthProviders 返回全部 provider 声明（副本，按声明顺序）。
func AuthProviders() []AuthProviderSpec {
	out := make([]AuthProviderSpec, len(authProviderSpecs))
	copy(out, authProviderSpecs)
	return out
}

// AuthProviderTypes 返回全部 provider 名，即 signup_source / provider_type 的合法取值。
func AuthProviderTypes() []string {
	out := make([]string, 0, len(authProviderSpecs))
	for _, spec := range authProviderSpecs {
		out = append(out, spec.Name)
	}
	return out
}

// IdentityBindingProviders 返回以 auth_identities 绑定到账号的第三方 provider 名。
func IdentityBindingProviders() []string {
	out := make([]string, 0, len(authProviderSpecs))
	for _, spec := range authProviderSpecs {
		if spec.BindsIdentity {
			out = append(out, spec.Name)
		}
	}
	return out
}

// IsAuthProviderType 判断 value 是否为已声明的 provider 名（精确匹配，不做大小写折叠）。
func IsAuthProviderType(value string) bool {
	for _, spec := range authProviderSpecs {
		if spec.Name == value {
			return true
		}
	}
	return false
}

// IsIdentityBindingProvider 判断 value 是否为以 auth_identities 绑定的第三方 provider。
func IsIdentityBindingProvider(value string) bool {
	for _, spec := range authProviderSpecs {
		if spec.Name == value {
			return spec.BindsIdentity
		}
	}
	return false
}

// validateAuthProviderType 是 provider_type / signup_source 列共用的 ent 校验器。
func validateAuthProviderType(value string) error {
	if IsAuthProviderType(value) {
		return nil
	}
	return fmt.Errorf("invalid auth provider type %q: must be one of %s", value, strings.Join(AuthProviderTypes(), ", "))
}

// AuthIdentity stores the canonical login identity for an account.
type AuthIdentity struct {
	ent.Schema
}

func (AuthIdentity) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "auth_identities"},
	}
}

func (AuthIdentity) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (AuthIdentity) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),
		field.String("provider_type").
			MaxLen(20).
			NotEmpty().
			Validate(validateAuthProviderType),
		field.String("provider_key").
			NotEmpty().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.String("provider_subject").
			NotEmpty().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Time("verified_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("issuer").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.JSON("metadata", map[string]any{}).
			Default(func() map[string]any { return map[string]any{} }).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
	}
}

func (AuthIdentity) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("auth_identities").
			Field("user_id").
			Required().
			Unique(),
		edge.To("channels", AuthIdentityChannel.Type).
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.To("adoption_decisions", IdentityAdoptionDecision.Type),
	}
}

func (AuthIdentity) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("provider_type", "provider_key", "provider_subject").Unique(),
		index.Fields("user_id"),
		index.Fields("user_id", "provider_type"),
	}
}
