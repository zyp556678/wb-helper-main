package upstream

import (
	"crypto/rand"
	"encoding/hex"
)

// randomHex 返回 n 字节的随机十六进制字符串（2n 个字符）。
// 用于生成请求标识，避免引入外部 UUID 依赖。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败属极端情况，退化用固定前缀保证不 panic
		for i := range buf {
			buf[i] = byte(i)
		}
	}
	return hex.EncodeToString(buf)
}
