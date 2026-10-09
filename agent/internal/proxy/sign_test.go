package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// 固定测试向量：与冻结稿 §2.4 的签名格式字面一致。
//
// 消息 = "v1\n<file_id>\n<e>"，HMAC-SHA256(key = sign_key 的 32 字节)，hex 输出。
// 期望值由**独立实现**（Python hmac + hashlib）算出，防止本仓库实现漂移后
// "自己验自己"通过。
const (
	testSignKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	testFileID     = "1AbCdEfGhIjKlMnOpQrStUvWxYz"
	testExpiry     = "1760000000"
	testSignature  = "9f13437c21616fe83d89c99adff47e5530dfe113788d000c5b9710e4af52b293"
)

func testSignKey(t *testing.T) []byte {
	t.Helper()
	key, err := hex.DecodeString(testSignKeyHex)
	if err != nil {
		t.Fatalf("测试密钥不是合法 hex：%v", err)
	}
	return key
}

func TestSignMessageFormatIsFrozen(t *testing.T) {
	if got := string(signMessage("abc123", "1700000000")); got != "v1\nabc123\n1700000000" {
		t.Fatalf("签名串必须逐字为 v1\\n<file_id>\\n<e>，实际 %q", got)
	}
}

func TestSignMatchesFrozenVector(t *testing.T) {
	key := testSignKey(t)
	if got := Sign(key, testFileID, testExpiry); got != testSignature {
		t.Fatalf("签名与冻结向量不符：\n得到 %s\n期望 %s", got, testSignature)
	}
}

// 独立复算：测试里自己拼一次消息与 HMAC，不调 signMessage/Sign 的实现路径。
func TestSignIndependentlyRecomputed(t *testing.T) {
	key := testSignKey(t)
	message := []byte("v1" + "\n" + testFileID + "\n" + testExpiry)
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	want := hex.EncodeToString(mac.Sum(nil))
	if got := Sign(key, testFileID, testExpiry); got != want {
		t.Fatalf("独立复算不符：\n得到 %s\n期望 %s", got, want)
	}
}

func TestVerifyAcceptsValidSignature(t *testing.T) {
	now := time.Unix(1759999000, 0)
	if !Verify(testSignKey(t), testFileID, testExpiry, testSignature, now) {
		t.Fatal("有效签名应通过")
	}
}

func TestVerifyRejects(t *testing.T) {
	key := testSignKey(t)
	now := time.Unix(1759999000, 0)
	cases := []struct {
		name    string
		fileID  string
		expiry  string
		sig     string
		now     time.Time
		comment string
	}{
		{"签名被篡改", testFileID, testExpiry, "9f13437c21616fe83d89c99adff47e5530dfe113788d000c5b9710e4af52b294", now, "改最后一位"},
		{"文件 id 被篡改", testFileID + "x", testExpiry, testSignature, now, "签名绑定文件"},
		{"有效期被改大", testFileID, "1860000000", testSignature, now, "签名绑定 e"},
		{"已过期", testFileID, testExpiry, testSignature, time.Unix(1760000001, 0), "e <= now"},
		{"整秒边界视为过期", testFileID, testExpiry, testSignature, time.Unix(1760000000, 0), "e > now 才有效"},
		{"缺 s", testFileID, testExpiry, "", now, "缺参"},
		{"缺 e", testFileID, "", testSignature, now, "缺参"},
		{"缺 file_id", "", testExpiry, testSignature, now, "缺参"},
		{"s 不是 hex", testFileID, testExpiry, "zzzz", now, "解码失败"},
		{"s 长度不足", testFileID, testExpiry, "9f13", now, "必须 32 字节"},
		{"e 不是整数", testFileID, "abc", testSignature, now, "解析失败"},
		{"e 为负数", testFileID, "-1", testSignature, now, "解析为负数且已过期"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if Verify(key, tc.fileID, tc.expiry, tc.sig, tc.now) {
				t.Fatalf("应被拒绝（%s）", tc.comment)
			}
		})
	}
}

func TestVerifyRejectsOtherAgentsKey(t *testing.T) {
	// 别的 agent 用自己 key 签的 URL，在本 agent 上必须无效（冻结稿 §2.4）。
	otherKey := make([]byte, 32)
	for i := range otherKey {
		otherKey[i] = byte(i + 1)
	}
	foreign := Sign(otherKey, testFileID, testExpiry)
	if Verify(testSignKey(t), testFileID, testExpiry, foreign, time.Unix(1759999000, 0)) {
		t.Fatal("其他 agent 密钥签出的 URL 不应通过")
	}
}
