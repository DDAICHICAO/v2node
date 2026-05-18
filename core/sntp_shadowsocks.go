package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const sntpShadowsocksCipherAES128GCM = "sntp-aes-128-gcm"

func normalizeShadowsocksCipher(cipher string) (string, bool) {
	if strings.EqualFold(cipher, sntpShadowsocksCipherAES128GCM) {
		return "aes-128-gcm", true
	}
	return cipher, false
}

func deriveSntpShadowsocksPassword(password string) string {
	sum := sha256.Sum256([]byte("sntp-shadowsocks-aead-v1\x00aes-128-gcm\x00" + password))
	return hex.EncodeToString(sum[:])
}
