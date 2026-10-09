package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// 客户端 URL 的签名格式由冻结稿 §2.4 冻结：
//
//	签名 = HMAC-SHA256(key = 该 agent 的 sign_key, msg = "v1\n<file_id>\n<e>") 的 hex
//
// `v1` 前缀是版本号：将来换算法时新旧 URL 可区分（现在只实现 v1）。
// **本文件是该格式在 agent 侧的唯一实现**——测试、本地 mock 都调这里，
// 不允许任何地方手拼签名串。
func signMessage(fileID, expiry string) []byte {
	return []byte("v1\n" + fileID + "\n" + expiry)
}

// Sign 计算 hex 签名。生产数据面只用 Verify；Sign 供测试
// （独立复算防格式漂移）与本地 mock master 生成签名 URL 使用。
func Sign(signKey []byte, fileID, expiry string) string {
	mac := hmac.New(sha256.New, signKey)
	mac.Write(signMessage(fileID, expiry))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify 校验签名与有效期（常数时间比较，见冻结稿 §9-3）。
//
// 任何一项不满足都返回 false：缺参、`e` 不是十进制整数、已过期、`s` 不是
// 32 字节 hex、密钥不符。调用方对**所有** false 回同一句 403 文案，
// 不区分失败原因（§2.5），避免泄漏有效期的存在性。
func Verify(signKey []byte, fileID, expiry, sig string, now time.Time) bool {
	if fileID == "" || expiry == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || exp <= now.Unix() {
		return false
	}
	provided, err := hex.DecodeString(sig)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, signKey)
	mac.Write(signMessage(fileID, expiry))
	return hmac.Equal(provided, mac.Sum(nil))
}
