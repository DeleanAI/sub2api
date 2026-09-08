// Package secretcipher 是全局密文格式与密钥环的唯一定义处。
//
// 系统里所有用 TOTP_ENCRYPTION_KEY 落库的密文（TOTP 密钥、渠道监控 API Key、
// 备份/图片存储的 S3 密钥、Ollama 会话、审计节点 Token）都经由这里加解密。
// 把格式、key-id 推导、多密钥解密顺序集中在一个包里，是为了让"换密钥"成为
// 一个有定义的操作：密文自带 key-id，新旧密钥可以同时有效，轮换工具据此判断
// 每一行该不该重写。
//
// 密文格式：
//
//	v1:<key-id>:<base64(nonce + ciphertext + tag)>
//
// key-id 是 sha256(key) 的前 16 个 hex 字符。没有前缀的值视为旧格式
// （base64(nonce + ciphertext + tag)，不知道用的哪把钥匙），解密时按
// 主密钥、历史密钥的顺序逐个尝试。
package secretcipher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KeySize 是 AES-256 密钥的字节数。
const KeySize = 32

const (
	formatVersion  = "v1"
	formatSep      = ":"
	keyIDHexLength = 16
)

// ErrUnknownKeyID 表示密文标注的 key-id 不在当前密钥环里。
// 典型场景：另一实例用了不同的 TOTP_ENCRYPTION_KEY，或轮换后旧密钥没有放进
// TOTP_ENCRYPTION_KEY_PREVIOUS。
var ErrUnknownKeyID = errors.New("ciphertext was encrypted with a key that is not configured")

// ErrNoKeyCanDecrypt 表示旧格式密文用密钥环里每一把钥匙都解不开。
var ErrNoKeyCanDecrypt = errors.New("ciphertext cannot be decrypted with any configured key")

// ParseHexKey 把 hex 编码的密钥解析成 32 字节原始密钥。
func ParseHexKey(hexKey string) ([]byte, error) {
	hexKey = strings.TrimSpace(hexKey)
	if hexKey == "" {
		return nil, errors.New("encryption key is empty")
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("encryption key is not valid hex: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must be %d bytes (%d hex chars), got %d bytes", KeySize, KeySize*2, len(key))
	}
	return key, nil
}

// Fingerprint 返回密钥的 sha256 hex。它只用于比对，不会泄露密钥本身。
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

// KeyID 返回密钥的短标识：指纹的前 16 个 hex 字符。写进每一条密文的前缀。
func KeyID(key []byte) string {
	return Fingerprint(key)[:keyIDHexLength]
}

// KeyIDOfFingerprint 把完整指纹裁成 key-id，便于日志与指纹对照。
func KeyIDOfFingerprint(fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if len(fingerprint) < keyIDHexLength {
		return fingerprint
	}
	return fingerprint[:keyIDHexLength]
}

// Ring 是一把主密钥加零或多把历史密钥。
//
// 加密永远用主密钥；解密按密文前缀找钥匙，没有前缀时依次尝试。
type Ring struct {
	keys    []ringKey // keys[0] 是主密钥
	byKeyID map[string]int
}

type ringKey struct {
	raw         []byte
	id          string
	fingerprint string
}

// NewRing 用 hex 编码的主密钥与历史密钥构造密钥环。
//
// 历史密钥与主密钥重复、或历史密钥之间重复都报错：这不是可以静默合并的输入，
// 通常意味着配置漏改（轮换后忘了把旧密钥从 PREVIOUS 里拿掉）。
func NewRing(primaryHex string, previousHex []string) (*Ring, error) {
	primary, err := ParseHexKey(primaryHex)
	if err != nil {
		return nil, fmt.Errorf("primary key: %w", err)
	}
	previous := make([][]byte, 0, len(previousHex))
	for i, raw := range previousHex {
		key, err := ParseHexKey(raw)
		if err != nil {
			return nil, fmt.Errorf("previous key #%d: %w", i+1, err)
		}
		previous = append(previous, key)
	}
	return NewRingFromKeys(primary, previous...)
}

// NewRingFromKeys 用原始字节构造密钥环。
func NewRingFromKeys(primary []byte, previous ...[]byte) (*Ring, error) {
	if len(primary) != KeySize {
		return nil, fmt.Errorf("primary key must be %d bytes, got %d", KeySize, len(primary))
	}
	ring := &Ring{byKeyID: make(map[string]int, 1+len(previous))}
	if err := ring.add(primary); err != nil {
		return nil, err
	}
	for i, key := range previous {
		if len(key) != KeySize {
			return nil, fmt.Errorf("previous key #%d must be %d bytes, got %d", i+1, KeySize, len(key))
		}
		if err := ring.add(key); err != nil {
			return nil, fmt.Errorf("previous key #%d: %w", i+1, err)
		}
	}
	return ring, nil
}

func (r *Ring) add(raw []byte) error {
	id := KeyID(raw)
	if existing, dup := r.byKeyID[id]; dup {
		if existing == 0 {
			return fmt.Errorf("key %s duplicates the primary key", id)
		}
		return fmt.Errorf("key %s is listed twice", id)
	}
	r.byKeyID[id] = len(r.keys)
	r.keys = append(r.keys, ringKey{
		raw:         append([]byte(nil), raw...),
		id:          id,
		fingerprint: Fingerprint(raw),
	})
	return nil
}

// PrimaryKeyID 返回主密钥的 key-id。
func (r *Ring) PrimaryKeyID() string { return r.keys[0].id }

// PrimaryFingerprint 返回主密钥的完整指纹。
func (r *Ring) PrimaryFingerprint() string { return r.keys[0].fingerprint }

// PreviousKeyIDs 返回历史密钥的 key-id（按配置顺序）。
func (r *Ring) PreviousKeyIDs() []string {
	ids := make([]string, 0, len(r.keys)-1)
	for _, key := range r.keys[1:] {
		ids = append(ids, key.id)
	}
	return ids
}

// Keys 返回密钥原始字节的副本，主密钥在前。供仍然需要裸密钥的旧格式
// （支付配置的 iv:tag:ct）逐把尝试。
func (r *Ring) Keys() [][]byte {
	out := make([][]byte, 0, len(r.keys))
	for _, key := range r.keys {
		out = append(out, append([]byte(nil), key.raw...))
	}
	return out
}

// KeyIDByFingerprint 查找指纹对应的密钥：返回 key-id 与它是否是主密钥。
func (r *Ring) KeyIDByFingerprint(fingerprint string) (keyID string, primary bool, ok bool) {
	fingerprint = strings.TrimSpace(fingerprint)
	for i, key := range r.keys {
		if key.fingerprint == fingerprint {
			return key.id, i == 0, true
		}
	}
	return "", false, false
}

// Encrypt 用主密钥加密并打上版本与 key-id 前缀。
func (r *Ring) Encrypt(plaintext string) (string, error) {
	sealed, err := seal(r.keys[0].raw, plaintext)
	if err != nil {
		return "", err
	}
	return formatVersion + formatSep + r.keys[0].id + formatSep + sealed, nil
}

// Decrypt 解密：带前缀的密文按 key-id 取钥匙；旧格式密文按主密钥、历史密钥
// 的顺序尝试。错误信息里带 key-id，让"密钥不一致"在日志里一眼可辨。
func (r *Ring) Decrypt(ciphertext string) (string, error) {
	keyID, payload, versioned := splitCiphertext(ciphertext)
	if versioned {
		idx, ok := r.byKeyID[keyID]
		if !ok {
			return "", fmt.Errorf("%w: key id %s (configured: %s)", ErrUnknownKeyID, keyID, strings.Join(r.allKeyIDs(), ","))
		}
		plaintext, err := open(r.keys[idx].raw, payload)
		if err != nil {
			return "", fmt.Errorf("decrypt with key %s: %w", keyID, err)
		}
		return plaintext, nil
	}
	var lastErr error
	for _, key := range r.keys {
		plaintext, err := open(key.raw, payload)
		if err == nil {
			return plaintext, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("%w (legacy format, tried keys %s): %v", ErrNoKeyCanDecrypt, strings.Join(r.allKeyIDs(), ","), lastErr)
}

// Rotate 把一条密文改写成主密钥下的新格式。
//
// 返回值 changed=false 表示密文已经是主密钥的新格式（或为空），无需重写；
// 这是轮换工具幂等的依据。解不开（未知 key-id、密钥丢失）直接返回错误，
// 不会把解不开的数据当成"无需处理"悄悄跳过。
// RotateOutcome 说明一次 Rotate 实际做了什么，供调用方分别计数与打点。
type RotateOutcome int

const (
	// RotateUnchanged：已经是主密钥写的当前格式密文。
	RotateUnchanged RotateOutcome = iota
	// RotateReEncrypted：旧密钥的密文，已用主密钥重写。
	RotateReEncrypted
	// RotateAdoptedLegacyPlaintext：无版本前缀且任何一把钥匙都解不开，
	// 按历史明文收编——直接加密存回。
	RotateAdoptedLegacyPlaintext
)

// Rotate 把一个存量值改写成主密钥写的当前格式密文。
//
// 关于历史明文：备份 S3 密钥、图片存储密钥这类字段历史上存过未加密的明文，读路径
// 至今仍容忍它们（见 decryptStoredSecret）。轮换若只会解密，这些行必然失败，而
// 一旦有任何一行失败，命令就不写新指纹；操作者按文档清掉 TOTP_ENCRYPTION_KEY_PREVIOUS
// 重启后，release 模式会因指纹对不上拒绝启动——每个副本都起不来。
//
// 判据与读路径完全一致：带版本前缀却解不开，说明密钥环缺了写它的那把钥匙，这是
// 真失败，必须报出来；没有前缀才可能是历史明文，收编它。真是"用失落密钥加密的
// 无前缀密文"时，这个值本来也已经读不出来了，收编不会让任何人多丢东西。
func (r *Ring) Rotate(ciphertext string) (rotated string, outcome RotateOutcome, err error) {
	if ciphertext == "" {
		return "", RotateUnchanged, nil
	}
	if keyID, _, versioned := splitCiphertext(ciphertext); versioned && keyID == r.keys[0].id {
		return ciphertext, RotateUnchanged, nil
	}
	plaintext, decErr := r.Decrypt(ciphertext)
	if decErr != nil {
		if IsVersioned(ciphertext) {
			return "", RotateUnchanged, decErr
		}
		adopted, encErr := r.Encrypt(ciphertext)
		if encErr != nil {
			return "", RotateUnchanged, encErr
		}
		return adopted, RotateAdoptedLegacyPlaintext, nil
	}
	rotated, err = r.Encrypt(plaintext)
	if err != nil {
		return "", RotateUnchanged, err
	}
	return rotated, RotateReEncrypted, nil
}

func (r *Ring) allKeyIDs() []string {
	ids := make([]string, 0, len(r.keys))
	for _, key := range r.keys {
		ids = append(ids, key.id)
	}
	return ids
}

// KeyIDOf 返回密文前缀里的 key-id；旧格式密文返回 ok=false。
func KeyIDOf(ciphertext string) (keyID string, ok bool) {
	keyID, _, ok = splitCiphertext(ciphertext)
	return keyID, ok
}

// IsVersioned 报告一个值是否带本包的密文前缀。
// 调用方用它区分"确实是密文但解不开"（密钥问题）和"根本不是密文"（历史明文）。
func IsVersioned(value string) bool {
	_, _, ok := splitCiphertext(value)
	return ok
}

func splitCiphertext(ciphertext string) (keyID, payload string, versioned bool) {
	parts := strings.SplitN(ciphertext, formatSep, 3)
	if len(parts) != 3 || parts[0] != formatVersion || len(parts[1]) != keyIDHexLength {
		return "", ciphertext, false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", ciphertext, false
	}
	return parts[1], parts[2], true
}

func seal(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func open(key []byte, payload string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	return gcm, nil
}
