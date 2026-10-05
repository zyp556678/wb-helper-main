package server

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// nowUnix 返回当前 Unix 秒。
func nowUnix() int64 { return time.Now().Unix() }

// randomID 返回 12 字节随机十六进制串，用于生成 chatcmpl-/call_ 前缀的标识。
func randomID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "000000000000000000000000"
	}
	return hex.EncodeToString(buf)
}
