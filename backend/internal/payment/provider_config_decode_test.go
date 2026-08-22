//go:build unit

package payment

import (
	"errors"
	"testing"
)

// 规则：渠道配置先按明文 JSON 读，再用每一把密钥尝试旧格式密文；都不行就报错而不是当空配置。
func TestDecodeProviderConfig(t *testing.T) {
	t.Parallel()
	oldKey := make([]byte, AES256KeySize)
	newKey := make([]byte, AES256KeySize)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
		newKey[i] = byte(0xFF - i)
	}
	legacy, err := Encrypt(`{"appId":"app-1"}`, oldKey) //nolint:staticcheck // 构造升级前的旧密文
	if err != nil {
		t.Fatalf("seed Encrypt: %v", err)
	}
	notJSON, err := Encrypt(`not json`, oldKey) //nolint:staticcheck // 构造升级前的旧密文
	if err != nil {
		t.Fatalf("seed Encrypt: %v", err)
	}

	tests := []struct {
		name       string
		stored     string
		keys       [][]byte
		want       map[string]string
		wantLegacy bool
		wantErr    bool
	}{
		{name: "empty", stored: "", keys: [][]byte{newKey}},
		{name: "plaintext", stored: `{"a":"b"}`, keys: nil, want: map[string]string{"a": "b"}},
		{name: "legacy_primary", stored: legacy, keys: [][]byte{oldKey}, want: map[string]string{"appId": "app-1"}, wantLegacy: true},
		{name: "legacy_previous_after_rotation", stored: legacy, keys: [][]byte{newKey, oldKey}, want: map[string]string{"appId": "app-1"}, wantLegacy: true},
		{name: "legacy_wrong_key", stored: legacy, keys: [][]byte{newKey}, wantErr: true},
		{name: "legacy_no_key", stored: legacy, keys: nil, wantErr: true},
		{name: "legacy_short_key_skipped", stored: legacy, keys: [][]byte{[]byte("short"), oldKey}, want: map[string]string{"appId": "app-1"}, wantLegacy: true},
		{name: "legacy_decrypts_to_garbage", stored: notJSON, keys: [][]byte{oldKey}, wantErr: true},
		{name: "garbage", stored: "not-json-and-not-ciphertext", keys: [][]byte{oldKey}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, legacy, err := DecodeProviderConfig(tt.stored, tt.keys)
			if tt.wantErr {
				if !errors.Is(err, ErrProviderConfigUnreadable) {
					t.Fatalf("err = %v, want ErrProviderConfigUnreadable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if legacy != tt.wantLegacy {
				t.Fatalf("legacy = %v, want %v", legacy, tt.wantLegacy)
			}
			if !stringMapEqual(got, tt.want) {
				t.Fatalf("config = %v, want %v", got, tt.want)
			}
		})
	}
}

// 轮换后：主密钥换了，旧密文仍能通过历史密钥读出；没有历史密钥则读不出但不会静默。
func TestDefaultLoadBalancerDecryptConfigUsesPreviousKeys(t *testing.T) {
	t.Parallel()
	oldKey := make([]byte, AES256KeySize)
	newKey := make([]byte, AES256KeySize)
	for i := range oldKey {
		oldKey[i] = byte(i + 7)
		newKey[i] = byte(0xA0 - i)
	}
	legacy, err := Encrypt(`{"k":"v"}`, oldKey) //nolint:staticcheck // 构造升级前的旧密文
	if err != nil {
		t.Fatalf("seed Encrypt: %v", err)
	}

	withPrevious := NewDefaultLoadBalancer(nil, newKey, oldKey)
	got, err := withPrevious.decryptConfig(legacy)
	if err != nil || !stringMapEqual(got, map[string]string{"k": "v"}) {
		t.Fatalf("decryptConfig with previous key = %v, %v", got, err)
	}

	withoutPrevious := NewDefaultLoadBalancer(nil, newKey)
	got, err = withoutPrevious.decryptConfig(legacy)
	if err != nil || got != nil {
		t.Fatalf("decryptConfig without previous key = %v, %v; want empty config, nil error", got, err)
	}

	if ids := keyIDs([][]byte{newKey, []byte("short"), oldKey}); len(ids) != 2 {
		t.Fatalf("keyIDs skipped nothing: %v", ids)
	}
}
