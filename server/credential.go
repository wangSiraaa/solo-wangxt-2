package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本地模拟的签名离线凭证:格式 v1.<base64url(payload)>.<base64url(hmac-sha256)>
// 仅用于演示,生产环境应替换为真正的许可证文件/非对称签名。

var (
	ErrCredentialFormat    = errors.New("credential_format")
	ErrCredentialSignature = errors.New("credential_signature")
)

type Claims struct {
	BorrowID  int64  `json:"bid"`
	PoolID    int64  `json:"pid"`
	Nonce     string `json:"nonce"`
	ExpUnixMs int64  `json:"exp"`
}

func SignCredential(secret []byte, c Claims) string {
	payload, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(p))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return "v1." + p + "." + sig
}

func ParseCredential(secret []byte, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, ErrCredentialFormat
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrCredentialFormat
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrCredentialFormat
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, ErrCredentialSignature
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrCredentialFormat
	}
	return &c, nil
}

func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
