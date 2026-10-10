package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 客户端 URL 的签名格式由冻结稿 §2.4 冻结，v2 是既定的版本演进路径
// （任务 10-10-agent-hub-direct-v2）：
//
//	v1: 签名 = HMAC-SHA256(key = 该 agent 的 sign_key, msg = "v1\n<file_id>\n<e>")
//	v2: 签名 = HMAC-SHA256(key = 该 agent 的 sign_key, msg = "v2\n<file_id>\n<e>\n<u>\n<f>")
//
// `v1` / `v2` 前缀是版本号：新旧 URL 由此可区分，两种校验长期共存（N5）。
// v2 在消息里多签了 u（hub 基址）与 f（Drive 文件 id）——这两个参数决定
// "这次请求从哪台 hub 取、取哪个文件"，不进签名就等于把一条合法 URL 拱手变成
// 任意文件 / 任意主机的代理（N1 的安全红线）。
//
// **本文件是两个格式在 agent 侧的唯一实现**——测试、本地 mock 都调这里，
// 不允许任何地方手拼签名串。
func signMessage(fileID, expiry string) []byte {
	return []byte("v1\n" + fileID + "\n" + expiry)
}

// v2 消息里的 u / f 是**解码后**的原值：master 用原值算 HMAC 再按查询参数
// 编码进 URL，agent 侧 Query() 解码后按原值复算 —— 两端不能有任何编码差异。
func signMessageV2(fileID, expiry, hubBase, driveFileID string) []byte {
	return []byte("v2\n" + fileID + "\n" + expiry + "\n" + hubBase + "\n" + driveFileID)
}

// normalizeHubBase 归一化并校验 v2 参数 u（hub 基址）。
//
// 形状要求与 master 侧 parsePublicBaseURL 同款：http/https 绝对地址、必须有
// 主机名、不接受 userinfo / 查询参数 / 片段 / 空白。归一化 = 去首尾空白 +
// 去结尾的 '/'（否则拼出的上游会多一层空路径段）。
//
// 返回 ok=false 时调用方按"校验失败"处理（与签名不符同一句 403）。
func normalizeHubBase(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false
	}
	// 内部空白(含换行)一律拒绝: 地址会被拼进一行 HTTP 请求行, 且与
	// agent-install.sh 里同一份校验的口径保持一致(那里不允许任何空白)。
	if strings.ContainsAny(value, " \t\r\n") {
		return "", false
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return "", false
	}
	if u.Hostname() == "" {
		return "", false
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(value, "/"), true
}

// validDriveFileID 校验 v2 参数 f（Drive 文件 id）。
//
// 它会被拼成 hub 数据面路径的一段（{u}/f/{f}）：必须非空、不含空白与 '/'
// （含 '/' 会把请求指到 hub 的其它路径上）。真实 Drive 文件 id 是
// [A-Za-z0-9_-] 形态，这两条永远不会误伤。
func validDriveFileID(raw string) bool {
	return raw != "" &&
		raw == strings.TrimSpace(raw) &&
		!strings.ContainsAny(raw, "/ \t\r\n")
}

// SignV2 计算 v2（hub 直连）签名。生产数据面只用 VerifyV2；SignV2 供测试
// （独立复算防格式漂移）与本地 mock master 生成签名 URL 使用。
func SignV2(signKey []byte, fileID, expiry, hubBase, driveFileID string) string {
	mac := hmac.New(sha256.New, signKey)
	mac.Write(signMessageV2(fileID, expiry, hubBase, driveFileID))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyV2 校验 v2 签名、有效期与 u/f 形态（常数时间比较，见冻结稿 §9-3）。
//
// 成功返回**归一化**后的 hub 基址，调用方据此拼上游地址；失败返回 ("", false)。
// 失败语义与 Verify 完全一致：缺参、`e` 不是十进制整数、已过期、`s` 不是
// 32 字节 hex、密钥不符、`u` 形态非法、`f` 非法。调用方对**所有** false 回
// 同一句 403 文案（§2.5），不区分失败原因。
//
// 注意：签名校验针对传入的 u/f **原值**（不做任何归一化），归一化只用于返回值
// —— 任何字节改动（哪怕只是加一个结尾 '/'）都会让签名失效。
func VerifyV2(signKey []byte, fileID, expiry, hubBase, driveFileID, sig string, now time.Time) (string, bool) {
	if fileID == "" || expiry == "" || sig == "" {
		return "", false
	}
	exp, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || exp <= now.Unix() {
		return "", false
	}
	normalized, ok := normalizeHubBase(hubBase)
	if !ok || !validDriveFileID(driveFileID) {
		return "", false
	}
	provided, err := hex.DecodeString(sig)
	if err != nil || len(provided) != sha256.Size {
		return "", false
	}
	mac := hmac.New(sha256.New, signKey)
	mac.Write(signMessageV2(fileID, expiry, hubBase, driveFileID))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return "", false
	}
	return normalized, true
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
