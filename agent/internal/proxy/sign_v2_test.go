package proxy

// v2(hub 直连)签名格式的冻结向量与校验矩阵(任务 10-10-agent-hub-direct-v2, N1/N5)。
//
// 期望值由【独立实现】(Python3 的 hmac + hashlib)算出, 而不是"用本仓库的代码
// 算一遍再抄过来"——否则实现漂移时"自己验自己"照样通过:
//
//	python3 - <<'PY'
//	import hmac, hashlib
//	key = bytes.fromhex("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
//	msg = "v2\n1AbCdEfGhIjKlMnOpQrStUvWxYz\n1760000000\nhttp://[2001:db8::1]:8791\n1HubFile-7"
//	print(hmac.new(key, msg.encode(), hashlib.sha256).hexdigest())
//	PY
//
// master 侧的 sign_internal_test.go 冻结同一份期望值(同名常量 v2Frozen*)：
// 两侧实现任一漂移都会在各自仓库的 CI 里炸。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

const (
	// v2FrozenFileID 与 v1 冻结向量相同的 token(agent 眼中的 file_id)
	v2FrozenFileID = "1AbCdEfGhIjKlMnOpQrStUvWxYz"
	// v2FrozenExpiry 与 v1 冻结向量相同的过期时间
	v2FrozenExpiry = "1760000000"
	// v2FrozenHubBase hub 内网基址(v6 形态: 顺带证明方括号地址也能签能验)
	v2FrozenHubBase = "http://[2001:db8::1]:8791"
	// v2FrozenDriveFileID Drive 文件 id
	v2FrozenDriveFileID = "1HubFile-7"
	// v2FrozenSignature 期望签名(小写 hex)
	v2FrozenSignature = "acd9f0f5ccd4825a2d5e20022325a664d111490fa59707ee82feee23070d520a"
)

func TestSignMessageV2FormatIsFrozen(t *testing.T) {
	got := string(signMessageV2("abc123", "1700000000", "http://10.0.0.9:8791", "drive-1"))
	want := "v2\nabc123\n1700000000\nhttp://10.0.0.9:8791\ndrive-1"
	if got != want {
		t.Fatalf("v2 签名串必须逐字为 %q, 实际 %q", want, got)
	}
}

func TestSignV2MatchesFrozenVector(t *testing.T) {
	got := SignV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID)
	if got != v2FrozenSignature {
		t.Fatalf("v2 签名与冻结向量不符:\n得到 %s\n期望 %s", got, v2FrozenSignature)
	}
}

// 独立复算: 测试里自己拼一次消息与 HMAC, 不调 signMessageV2/SignV2 的实现路径。
func TestSignV2IndependentlyRecomputed(t *testing.T) {
	message := []byte("v2" + "\n" + v2FrozenFileID + "\n" + v2FrozenExpiry + "\n" +
		v2FrozenHubBase + "\n" + v2FrozenDriveFileID)
	mac := hmac.New(sha256.New, testSignKey(t))
	mac.Write(message)
	if got := hex.EncodeToString(mac.Sum(nil)); got != v2FrozenSignature {
		t.Fatalf("独立复算不符:\n得到 %s\n期望 %s", got, v2FrozenSignature)
	}
}

// v1 与 v2 是两条互不相认的通道: 同一份 (file_id, e) 下 v1 签名与 v2 签名不同,
// 且各自的校验只认自己那条消息(版本前缀进签名)。
func TestVerifyV2RejectsV1Signature(t *testing.T) {
	v1Sig := Sign(testSignKey(t), v2FrozenFileID, v2FrozenExpiry)
	if v1Sig == v2FrozenSignature {
		t.Fatal("v1/v2 签名不应相同(消息不同)")
	}
	now := time.Unix(1759999000, 0)

	// v1 签名放在 v2 参数位: 必须拒绝
	if _, ok := VerifyV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry,
		v2FrozenHubBase, v2FrozenDriveFileID, v1Sig, now); ok {
		t.Fatal("v1 签名不应通过 v2 校验")
	}
	// v2 签名放在 v1 校验位: 必须拒绝(v1 路径零变化, 不认新消息)
	if Verify(testSignKey(t), v2FrozenFileID, v2FrozenExpiry, v2FrozenSignature, now) {
		t.Fatal("v2 签名不应通过 v1 校验")
	}
}

func TestVerifyV2AcceptsValidSignature(t *testing.T) {
	now := time.Unix(1759999000, 0)
	base, ok := VerifyV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry,
		v2FrozenHubBase, v2FrozenDriveFileID, v2FrozenSignature, now)
	if !ok {
		t.Fatal("有效 v2 签名应通过")
	}
	if base != v2FrozenHubBase {
		t.Fatalf("归一化后的 hub 基址 = %q, want %q", base, v2FrozenHubBase)
	}
}

// u 的结尾 '/' 归一化: 签名仍按**原值**校验(多一个字符签名即失效), 但成功
// 返回的基址不带结尾斜杠(否则拼出的上游会多一层空路径段)。
func TestVerifyV2NormalizesTrailingSlashAfterVerify(t *testing.T) {
	raw := "http://10.0.0.9:8791/"
	sig := SignV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry, raw, v2FrozenDriveFileID)
	base, ok := VerifyV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry,
		raw, v2FrozenDriveFileID, sig, time.Unix(1759999000, 0))
	if !ok {
		t.Fatal("带结尾斜杠的原值(签名覆盖原值)应通过")
	}
	if base != "http://10.0.0.9:8791" {
		t.Fatalf("返回值应去掉结尾斜杠, 实际 %q", base)
	}

	// 原值带斜杠、签名按去掉斜杠的版本计算 → 必须拒绝(签名绑定原始字节)
	sigNoSlash := SignV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry, "http://10.0.0.9:8791", v2FrozenDriveFileID)
	if _, ok := VerifyV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry,
		raw, v2FrozenDriveFileID, sigNoSlash, time.Unix(1759999000, 0)); ok {
		t.Fatal("签名必须绑定 u 的原始字节, 结尾斜杠也算改动")
	}
}

// TestVerifyV2TamperMatrix 篡改矩阵(A1): u / f / e / file_id 任一字节改动、
// 跨 agent 密钥、缺参、非法 u 形态、非法 f, 全部必须失败。
//
// 非法 u/f 形态的用例都用**正确的密钥签名**: 走的只能是形态校验那条路,
// 证明"合法签名 + 非法参数"同样被拦(N1: 非法 → 视为校验失败)。
func TestVerifyV2TamperMatrix(t *testing.T) {
	key := testSignKey(t)
	now := time.Unix(1759999000, 0)

	type params struct {
		fileID, expiry, hubBase, driveFileID, sig string
	}
	sign := func(p params) string {
		return SignV2(key, p.fileID, p.expiry, p.hubBase, p.driveFileID)
	}
	// 形态非法用例: u/f 可以任意畸形, 但签名按"原样"计算 —— 走不到签名那步,
	// 只能被形态校验拦下(证明合法签名 + 非法参数同样 403)。
	signRaw := func(hubBase, driveFileID string) string {
		return SignV2(key, v2FrozenFileID, v2FrozenExpiry, hubBase, driveFileID)
	}
	base := params{
		fileID:      v2FrozenFileID,
		expiry:      v2FrozenExpiry,
		hubBase:     v2FrozenHubBase,
		driveFileID: v2FrozenDriveFileID,
	}
	base.sig = sign(base)

	cases := []struct {
		name string
		p    params
		now  time.Time
	}{
		{"u 端口被篡改", params{v2FrozenFileID, v2FrozenExpiry, "http://[2001:db8::1]:8792", v2FrozenDriveFileID, base.sig}, now},
		{"u 主机被篡改", params{v2FrozenFileID, v2FrozenExpiry, "http://[2001:db8::2]:8791", v2FrozenDriveFileID, base.sig}, now},
		{"f 被篡改", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID + "x", base.sig}, now},
		{"f 换一个文件", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, "1HubFile-8", base.sig}, now},
		{"e 被改大", params{v2FrozenFileID, "1860000000", v2FrozenHubBase, v2FrozenDriveFileID, base.sig}, now},
		{"file_id 被篡改", params{v2FrozenFileID + "x", v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID, base.sig}, now},
		{"已过期", base, time.Unix(1760000001, 0)},
		{"整秒边界视为过期", base, time.Unix(1760000000, 0)},
		{"缺 s", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID, ""}, now},
		{"缺 e", params{v2FrozenFileID, "", v2FrozenHubBase, v2FrozenDriveFileID, base.sig}, now},
		{"缺 file_id", params{"", v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID, base.sig}, now},
		{"缺 u", params{v2FrozenFileID, v2FrozenExpiry, "", v2FrozenDriveFileID, base.sig}, now},
		{"缺 f", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, "", base.sig}, now},
		{"s 不是 hex", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID, "zzzz"}, now},
		{"s 长度不足", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID, "acd9"}, now},
		{"e 不是整数", params{v2FrozenFileID, "abc", v2FrozenHubBase, v2FrozenDriveFileID, base.sig}, now},

		// 非法 u 形态(签名正确, 只能被形态校验拦下)
		{"u 没有 scheme", params{v2FrozenFileID, v2FrozenExpiry, "10.0.0.9:8791", v2FrozenDriveFileID, signRaw("10.0.0.9:8791", v2FrozenDriveFileID)}, now},
		{"u scheme 非 http(s)", params{v2FrozenFileID, v2FrozenExpiry, "ftp://10.0.0.9:8791", v2FrozenDriveFileID, signRaw("ftp://10.0.0.9:8791", v2FrozenDriveFileID)}, now},
		{"u 没有主机", params{v2FrozenFileID, v2FrozenExpiry, "http://:8791", v2FrozenDriveFileID, signRaw("http://:8791", v2FrozenDriveFileID)}, now},
		{"u 只有 scheme", params{v2FrozenFileID, v2FrozenExpiry, "http://", v2FrozenDriveFileID, signRaw("http://", v2FrozenDriveFileID)}, now},
		{"u 带 userinfo", params{v2FrozenFileID, v2FrozenExpiry, "http://user:pw@10.0.0.9:8791", v2FrozenDriveFileID, signRaw("http://user:pw@10.0.0.9:8791", v2FrozenDriveFileID)}, now},
		{"u 带查询参数", params{v2FrozenFileID, v2FrozenExpiry, "http://10.0.0.9:8791?x=1", v2FrozenDriveFileID, signRaw("http://10.0.0.9:8791?x=1", v2FrozenDriveFileID)}, now},
		{"u 带片段", params{v2FrozenFileID, v2FrozenExpiry, "http://10.0.0.9:8791#frag", v2FrozenDriveFileID, signRaw("http://10.0.0.9:8791#frag", v2FrozenDriveFileID)}, now},
		{"u 内部有空格", params{v2FrozenFileID, v2FrozenExpiry, "http://10.0.0.9 :8791", v2FrozenDriveFileID, signRaw("http://10.0.0.9 :8791", v2FrozenDriveFileID)}, now},
		{"u 内部有换行", params{v2FrozenFileID, v2FrozenExpiry, "http://10.0.0.9:8791\nx", v2FrozenDriveFileID, signRaw("http://10.0.0.9:8791\nx", v2FrozenDriveFileID)}, now},

		// 非法 f 形态
		{"f 含斜杠", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, "a/b", signRaw(v2FrozenHubBase, "a/b")}, now},
		{"f 含空格", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, "a b", signRaw(v2FrozenHubBase, "a b")}, now},
		{"f 全空白", params{v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, "  ", signRaw(v2FrozenHubBase, "  ")}, now},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if base2, ok := VerifyV2(key, tc.p.fileID, tc.p.expiry, tc.p.hubBase, tc.p.driveFileID, tc.p.sig, tc.now); ok {
				t.Fatalf("应被拒绝, 实际通过(基址 %q)", base2)
			}
		})
	}
}

// TestVerifyV2RejectsOtherAgentsKey 跨 agent 密钥: 别的节点签出的 v2 URL 不认。
func TestVerifyV2RejectsOtherAgentsKey(t *testing.T) {
	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(i + 1)
	}
	foreign := SignV2(otherKey, v2FrozenFileID, v2FrozenExpiry, v2FrozenHubBase, v2FrozenDriveFileID)
	if _, ok := VerifyV2(testSignKey(t), v2FrozenFileID, v2FrozenExpiry,
		v2FrozenHubBase, v2FrozenDriveFileID, foreign, time.Unix(1759999000, 0)); ok {
		t.Fatal("其他 agent 密钥签出的 v2 URL 不应通过")
	}
}

// TestNormalizeHubBase u 形态校验与归一化矩阵(与 master 侧 parsePublicBaseURL 同款)。
func TestNormalizeHubBase(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"v4 带端口", "http://10.0.0.9:8791", "http://10.0.0.9:8791", true},
		{"https", "https://hub.example.com", "https://hub.example.com", true},
		{"v6 带方括号", "http://[2001:db8::1]:8791", "http://[2001:db8::1]:8791", true},
		{"去掉结尾斜杠", "http://10.0.0.9:8791/", "http://10.0.0.9:8791", true},
		{"去掉首尾空白", "  http://10.0.0.9:8791  ", "http://10.0.0.9:8791", true},
		{"scheme 大小写不敏感", "HTTP://10.0.0.9:8791", "HTTP://10.0.0.9:8791", true},
		{"带路径(主机不变, 允许)", "http://10.0.0.9:8791/proxy", "http://10.0.0.9:8791/proxy", true},

		{"空串", "", "", false},
		{"全空白", "   ", "", false},
		{"没有 scheme", "10.0.0.9:8791", "", false},
		{"scheme 非 http(s)", "ftp://10.0.0.9:8791", "", false},
		{"没有主机", "http://:8791", "", false},
		{"只有 scheme", "http://", "", false},
		{"相对地址", "/f/abc", "", false},
		{"带 userinfo", "http://user:pw@10.0.0.9:8791", "", false},
		{"带查询参数", "http://10.0.0.9:8791?x=1", "", false},
		{"带片段", "http://10.0.0.9:8791#frag", "", false},
		{"内部空格", "http://10.0.0.9 :8791", "", false},
		{"内部换行", "http://10.0.0.9\n:8791", "", false},
		{"结尾换行(按首尾空白裁掉, 合法)", "http://10.0.0.9:8791\n", "http://10.0.0.9:8791", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeHubBase(tc.raw)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (raw=%q)", ok, tc.ok, tc.raw)
			}
			if ok && got != tc.want {
				t.Fatalf("归一化结果 = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidDriveFileID(t *testing.T) {
	valid := []string{"1AbCdEfGhIjKlMnOpQrStUvWxYz", "1HubFile-7", "abc_DEF-123"}
	for _, id := range valid {
		if !validDriveFileID(id) {
			t.Errorf("合法 Drive 文件 id 被拒: %q", id)
		}
	}
	invalid := []string{"", " ", "a/b", "a b", "a\tb", "a\nb", " a", "a "}
	for _, id := range invalid {
		if validDriveFileID(id) {
			t.Errorf("非法 Drive 文件 id 被接受: %q", id)
		}
	}
}

// hubDirectLink 的拼法与 master 侧 hubFileURL 一致({base}/f/{PathEscape(f)})。
func TestHubDirectLinkShape(t *testing.T) {
	link := hubDirectLink("http://10.0.0.9:8791", "1AbC-D_ef", v2FrozenExpiry)
	if link.URL != "http://10.0.0.9:8791/f/1AbC-D_ef" {
		t.Fatalf("上游地址 = %q", link.URL)
	}
	if len(link.Headers) != 0 {
		t.Fatalf("hub 直连不带任何凭据, 实际: %v", link.Headers)
	}
	if !link.ExpiresAt.Equal(time.Unix(1760000000, 0)) {
		t.Fatalf("ExpiresAt = %v, want 1760000000", link.ExpiresAt)
	}
}

// 日志铁律(冻结稿 §3.7): 直连上游地址(u)与签名(s)不得进日志——这里只钉住
// "换链判定/基址归一化"这条路径不产生任何日志副作用。
func TestHubDirectLinkKeepsSecretsOutOfLogs(t *testing.T) {
	link := hubDirectLink("http://10.0.0.9:8791", "1AbC", v2FrozenExpiry)
	for name, value := range link.Headers {
		if strings.Contains(name, "Authorization") || strings.Contains(value, "Bearer") {
			t.Fatal("hub 直连上游不应带凭据头")
		}
	}
}
