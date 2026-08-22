package secretcipher

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hexKey(b byte) string {
	raw := make([]byte, KeySize)
	for i := range raw {
		raw[i] = b
	}
	return hex.EncodeToString(raw)
}

func mustRing(t *testing.T, primary string, previous ...string) *Ring {
	t.Helper()
	ring, err := NewRing(primary, previous)
	require.NoError(t, err)
	return ring
}

func TestParseHexKey(t *testing.T) {
	key, err := ParseHexKey(" " + hexKey(0x01) + " ")
	require.NoError(t, err)
	require.Len(t, key, KeySize)

	for name, raw := range map[string]string{
		"empty":        "",
		"odd_hex":      "abc",
		"not_hex":      strings.Repeat("zz", KeySize),
		"too_short":    hex.EncodeToString(make([]byte, 16)),
		"too_long":     hex.EncodeToString(make([]byte, 33)),
		"only_spaces":  "   ",
		"half_garbage": hexKey(0x01)[:40] + "xy" + hexKey(0x01)[42:],
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHexKey(raw)
			require.Error(t, err)
		})
	}
}

func TestKeyIDAndFingerprintAreDerivedFromKeyBytes(t *testing.T) {
	key, _ := ParseHexKey(hexKey(0x42))
	fp := Fingerprint(key)
	require.Len(t, fp, 64)
	require.Equal(t, fp[:16], KeyID(key))
	require.Equal(t, KeyID(key), KeyIDOfFingerprint(fp))
	require.Equal(t, "short", KeyIDOfFingerprint("short"))

	other, _ := ParseHexKey(hexKey(0x43))
	require.NotEqual(t, KeyID(key), KeyID(other))
}

func TestNewRingRejectsBadInput(t *testing.T) {
	_, err := NewRing("", nil)
	require.ErrorContains(t, err, "primary key")

	_, err = NewRing(hexKey(0x01), []string{"nope"})
	require.ErrorContains(t, err, "previous key #1")

	_, err = NewRing(hexKey(0x01), []string{hexKey(0x01)})
	require.ErrorContains(t, err, "duplicates the primary key")

	_, err = NewRing(hexKey(0x01), []string{hexKey(0x02), hexKey(0x02)})
	require.ErrorContains(t, err, "listed twice")

	_, err = NewRingFromKeys([]byte("short"))
	require.Error(t, err)
	_, err = NewRingFromKeys(make([]byte, KeySize), []byte("short"))
	require.Error(t, err)
}

func TestEncryptProducesVersionedPrefixWithPrimaryKeyID(t *testing.T) {
	ring := mustRing(t, hexKey(0x01), hexKey(0x02))
	ct, err := ring.Encrypt("hello")
	require.NoError(t, err)

	parts := strings.SplitN(ct, ":", 3)
	require.Len(t, parts, 3)
	require.Equal(t, "v1", parts[0])
	require.Equal(t, ring.PrimaryKeyID(), parts[1])
	_, err = base64.StdEncoding.DecodeString(parts[2])
	require.NoError(t, err)

	id, ok := KeyIDOf(ct)
	require.True(t, ok)
	require.Equal(t, ring.PrimaryKeyID(), id)
	require.True(t, IsVersioned(ct))
	require.False(t, IsVersioned("plain-secret"))
	require.False(t, IsVersioned("v1:notahexid:payload"))
	require.False(t, IsVersioned("v2:"+ring.PrimaryKeyID()+":payload"))
}

func TestRoundTrip(t *testing.T) {
	ring := mustRing(t, hexKey(0x01))
	for _, plaintext := range []string{"", "ascii", "多字节 UTF-8 文本", strings.Repeat("x", 4096), "with:colons:inside"} {
		ct, err := ring.Encrypt(plaintext)
		require.NoError(t, err)
		got, err := ring.Decrypt(ct)
		require.NoError(t, err)
		assert.Equal(t, plaintext, got)
	}
}

func TestNonceIsRandomPerEncrypt(t *testing.T) {
	ring := mustRing(t, hexKey(0x01))
	seen := map[string]struct{}{}
	for i := 0; i < 20; i++ {
		ct, err := ring.Encrypt("same")
		require.NoError(t, err)
		seen[ct] = struct{}{}
	}
	require.Len(t, seen, 20)
}

func TestDecryptLegacyFormatTriesPrimaryThenPrevious(t *testing.T) {
	old := mustRing(t, hexKey(0x02))
	legacy, err := seal(old.keys[0].raw, "legacy-secret")
	require.NoError(t, err)
	require.False(t, IsVersioned(legacy))

	// 只有主密钥：解不开旧密钥的旧格式密文，错误要点名尝试过的 key-id。
	onlyNew := mustRing(t, hexKey(0x01))
	_, err = onlyNew.Decrypt(legacy)
	require.ErrorIs(t, err, ErrNoKeyCanDecrypt)
	require.ErrorContains(t, err, onlyNew.PrimaryKeyID())

	// 旧密钥在历史列表里：解得开。
	withPrevious := mustRing(t, hexKey(0x01), hexKey(0x02))
	got, err := withPrevious.Decrypt(legacy)
	require.NoError(t, err)
	require.Equal(t, "legacy-secret", got)

	// 旧格式但主密钥就能解开：也不需要历史密钥。
	legacyPrimary, err := seal(withPrevious.keys[0].raw, "primary-legacy")
	require.NoError(t, err)
	got, err = withPrevious.Decrypt(legacyPrimary)
	require.NoError(t, err)
	require.Equal(t, "primary-legacy", got)
}

func TestDecryptVersionedWithPreviousKey(t *testing.T) {
	old := mustRing(t, hexKey(0x02))
	ct, err := old.Encrypt("rotate-me")
	require.NoError(t, err)

	ring := mustRing(t, hexKey(0x01), hexKey(0x02))
	got, err := ring.Decrypt(ct)
	require.NoError(t, err)
	require.Equal(t, "rotate-me", got)
}

func TestDecryptUnknownKeyIDNamesTheKey(t *testing.T) {
	stranger := mustRing(t, hexKey(0x09))
	ct, err := stranger.Encrypt("secret")
	require.NoError(t, err)

	ring := mustRing(t, hexKey(0x01), hexKey(0x02))
	_, err = ring.Decrypt(ct)
	require.ErrorIs(t, err, ErrUnknownKeyID)
	require.ErrorContains(t, err, stranger.PrimaryKeyID())
	require.ErrorContains(t, err, ring.PrimaryKeyID())
}

func TestDecryptRejectsTamperedAndMalformed(t *testing.T) {
	ring := mustRing(t, hexKey(0x01))
	ct, err := ring.Encrypt("payload")
	require.NoError(t, err)

	parts := strings.SplitN(ct, ":", 3)
	raw, err := base64.StdEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0xFF
	tampered := parts[0] + ":" + parts[1] + ":" + base64.StdEncoding.EncodeToString(raw)
	_, err = ring.Decrypt(tampered)
	require.ErrorContains(t, err, "decrypt with key "+ring.PrimaryKeyID())

	_, err = ring.Decrypt("!!!not base64!!!")
	require.Error(t, err)
	_, err = ring.Decrypt(base64.StdEncoding.EncodeToString([]byte{1, 2}))
	require.Error(t, err)
}

func TestRotateIsIdempotentAndFailsLoudly(t *testing.T) {
	old := mustRing(t, hexKey(0x02))
	ring := mustRing(t, hexKey(0x01), hexKey(0x02))

	// 空值：无事可做。
	out, changed, err := ring.Rotate("")
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, out)

	// 旧密钥的新格式密文：重写到主密钥。
	ct, err := old.Encrypt("secret")
	require.NoError(t, err)
	rotated, changed, err := ring.Rotate(ct)
	require.NoError(t, err)
	require.True(t, changed)
	id, _ := KeyIDOf(rotated)
	require.Equal(t, ring.PrimaryKeyID(), id)
	plain, err := ring.Decrypt(rotated)
	require.NoError(t, err)
	require.Equal(t, "secret", plain)

	// 再轮一次：已经是主密钥，不动。
	again, changed, err := ring.Rotate(rotated)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, rotated, again)

	// 旧格式密文：升级成带 key-id 的新格式。
	legacy, err := seal(old.keys[0].raw, "legacy")
	require.NoError(t, err)
	upgraded, changed, err := ring.Rotate(legacy)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, IsVersioned(upgraded))

	// 未知密钥：报错而不是跳过。
	stranger := mustRing(t, hexKey(0x09))
	foreign, err := stranger.Encrypt("x")
	require.NoError(t, err)
	_, _, err = ring.Rotate(foreign)
	require.ErrorIs(t, err, ErrUnknownKeyID)
}

func TestRingIntrospection(t *testing.T) {
	ring := mustRing(t, hexKey(0x01), hexKey(0x02), hexKey(0x03))
	require.Len(t, ring.PreviousKeyIDs(), 2)
	require.Len(t, ring.Keys(), 3)
	require.Equal(t, hexKey(0x01), hex.EncodeToString(ring.Keys()[0]))

	id, primary, ok := ring.KeyIDByFingerprint(ring.PrimaryFingerprint())
	require.True(t, ok)
	require.True(t, primary)
	require.Equal(t, ring.PrimaryKeyID(), id)

	second, _ := ParseHexKey(hexKey(0x02))
	id, primary, ok = ring.KeyIDByFingerprint(Fingerprint(second))
	require.True(t, ok)
	require.False(t, primary)
	require.Equal(t, KeyID(second), id)

	_, _, ok = ring.KeyIDByFingerprint("deadbeef")
	require.False(t, ok)

	// Keys() 返回副本：改它不影响密钥环。
	ring.Keys()[0][0] ^= 0xFF
	_, err := ring.Decrypt(mustEncrypt(t, ring, "still-works"))
	require.NoError(t, err)
}

func mustEncrypt(t *testing.T, ring *Ring, plaintext string) string {
	t.Helper()
	ct, err := ring.Encrypt(plaintext)
	require.NoError(t, err)
	return ct
}
