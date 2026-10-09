// Package cryptos 提供安全场景的通用工具: 加密安全的随机串与常数时间比较
//
// 与 randoms 包的区别: randoms.RandomHex 基于 math/rand, 只适合无安全要求的
// 随机展示; 本包的随机串来自 crypto/rand, 是密钥 / 令牌的唯一来源, 两者不可混用。
package cryptos

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
)

// RandomHex 生成 nBytes 个随机字节, 返回 2*nBytes 位的小写 16 进制字符串
//
// 与 randoms.RandomHex 的参数含义不同: 这里的 n 是【字节数】而不是字符数,
// 便于直接对齐协议里的密钥长度约定(如 secrets.token_hex(32) 的 64 位 hex)。
//
// 随机源为 crypto/rand, 失败时返回空字符串(util 层惯例: 失败返回零值): 调用方
// 必须把空串当成内部错误处理 —— 绝不落库, 绝不下发给节点。
func RandomHex(nBytes int) string {
	if nBytes <= 0 {
		return ""
	}
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// Equal 以常数时间比较两枚凭据是否相等
//
// 长度不同直接返回 false(长度本身不是秘密); 长度相同时用 subtle.ConstantTimeCompare,
// 避免逐字符比较泄漏"前几位猜对了"的时间差。
//
// 任一侧为空都返回 false: 空凭据永远不相等, 防止"配置漏填 + 请求漏传"这类
// 双向缺失被判定为匹配。
func Equal(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
