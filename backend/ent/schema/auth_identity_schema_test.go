package schema

import (
	"testing"

	"entgo.io/ent"
	"entgo.io/ent/entc/load"
	"entgo.io/ent/schema/field"
	"github.com/stretchr/testify/require"
)

func TestAuthIdentityFoundationSchemas(t *testing.T) {
	spec, err := (&load.Config{Path: "."}).Load()
	require.NoError(t, err)

	schemas := map[string]*load.Schema{}
	for _, schema := range spec.Schemas {
		schemas[schema.Name] = schema
	}

	authIdentity := requireSchema(t, schemas, "AuthIdentity")
	requireSchemaFields(t, authIdentity,
		"user_id",
		"provider_type",
		"provider_key",
		"provider_subject",
		"verified_at",
		"issuer",
		"metadata",
	)
	requireHasUniqueIndex(t, authIdentity, "provider_type", "provider_key", "provider_subject")

	authIdentityChannel := requireSchema(t, schemas, "AuthIdentityChannel")
	requireSchemaFields(t, authIdentityChannel,
		"identity_id",
		"provider_type",
		"provider_key",
		"channel",
		"channel_app_id",
		"channel_subject",
		"metadata",
	)
	requireHasUniqueIndex(t, authIdentityChannel, "provider_type", "provider_key", "channel", "channel_app_id", "channel_subject")

	pendingAuthSession := requireSchema(t, schemas, "PendingAuthSession")
	requireSchemaFields(t, pendingAuthSession,
		"intent",
		"provider_type",
		"provider_key",
		"provider_subject",
		"target_user_id",
		"redirect_to",
		"resolved_email",
		"registration_password_hash",
		"upstream_identity_claims",
		"local_flow_state",
		"browser_session_key",
		"completion_code_hash",
		"completion_code_expires_at",
		"email_verified_at",
		"password_verified_at",
		"totp_verified_at",
		"expires_at",
		"consumed_at",
	)

	adoptionDecision := requireSchema(t, schemas, "IdentityAdoptionDecision")
	requireSchemaFields(t, adoptionDecision,
		"pending_auth_session_id",
		"identity_id",
		"adopt_display_name",
		"adopt_avatar",
		"decided_at",
	)
	requireHasUniqueIndex(t, adoptionDecision, "pending_auth_session_id")

	userSchema := requireSchema(t, schemas, "User")
	requireSchemaFields(t, userSchema, "signup_source", "last_login_at", "last_active_at")
	signupSource := requireSchemaField(t, userSchema, "signup_source")
	require.Equal(t, field.TypeString, signupSource.Info.Type)
	require.True(t, signupSource.Default)
	require.Equal(t, "email", signupSource.DefaultValue)
	require.Equal(t, 1, signupSource.Validators)

	// signup_source 与 auth_identities.provider_type 共用同一份 provider 声明：
	// 声明里的每个 provider 都必须通过，未声明的必须被拒。
	validator := requireStringFieldValidator(t, User{}.Fields(), "signup_source")
	providerTypeValidator := requireStringFieldValidator(t, AuthIdentity{}.Fields(), "provider_type")
	require.NotEmpty(t, AuthProviderTypes())
	for _, value := range AuthProviderTypes() {
		require.NoError(t, validator(value))
		require.NoError(t, providerTypeValidator(value))
	}
	require.Error(t, validator("unknown"))
	require.Error(t, providerTypeValidator("unknown"))
	require.Error(t, validator("Feishu"), "provider names are exact, no case folding")
}

// TestAuthProviderDeclaration 固化声明本身的形状：名字非空且唯一、email 不可绑定、
// 第三方绑定 provider 都在 AuthProviderTypes 里。
func TestAuthProviderDeclaration(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range AuthProviders() {
		require.NotEmpty(t, spec.Name)
		require.False(t, seen[spec.Name], "duplicate provider %q", spec.Name)
		seen[spec.Name] = true
		require.Equal(t, spec.BindsIdentity, IsIdentityBindingProvider(spec.Name))
		require.True(t, IsAuthProviderType(spec.Name))
	}
	require.True(t, seen["email"])
	require.False(t, IsIdentityBindingProvider("email"), "email is the local account itself, never a bindable third party")
	require.False(t, IsIdentityBindingProvider("unknown"))
	for _, name := range IdentityBindingProviders() {
		require.True(t, IsAuthProviderType(name))
		require.NotEqual(t, "email", name)
	}
}

func requireSchema(t *testing.T, schemas map[string]*load.Schema, name string) *load.Schema {
	t.Helper()

	schema, ok := schemas[name]
	require.True(t, ok, "schema %s should exist", name)
	return schema
}

func requireSchemaFields(t *testing.T, schema *load.Schema, names ...string) {
	t.Helper()

	fields := map[string]struct{}{}
	for _, field := range schema.Fields {
		fields[field.Name] = struct{}{}
	}

	for _, name := range names {
		_, ok := fields[name]
		require.True(t, ok, "schema %s should include field %s", schema.Name, name)
	}
}

func requireSchemaField(t *testing.T, schema *load.Schema, name string) *load.Field {
	t.Helper()

	for _, schemaField := range schema.Fields {
		if schemaField.Name == name {
			return schemaField
		}
	}

	require.Failf(t, "missing schema field", "schema %s should include field %s", schema.Name, name)
	return nil
}

func requireStringFieldValidator(t *testing.T, fields []ent.Field, name string) func(string) error {
	t.Helper()

	for _, entField := range fields {
		descriptor := entField.Descriptor()
		if descriptor.Name != name {
			continue
		}
		require.NotEmpty(t, descriptor.Validators, "field %s should include a validator", name)
		validator, ok := descriptor.Validators[0].(func(string) error)
		require.True(t, ok, "field %s validator should be func(string) error", name)
		return validator
	}

	require.Failf(t, "missing field validator", "schema should include field %s", name)
	return nil
}

func requireHasUniqueIndex(t *testing.T, schema *load.Schema, fields ...string) {
	t.Helper()

	for _, index := range schema.Indexes {
		if !index.Unique {
			continue
		}
		if len(index.Fields) != len(fields) {
			continue
		}
		match := true
		for i := range fields {
			if index.Fields[i] != fields[i] {
				match = false
				break
			}
		}
		if match {
			return
		}
	}

	require.Failf(t, "missing unique index", "schema %s should include unique index on %v", schema.Name, fields)
}
