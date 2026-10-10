package agentnet

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"

	"gopkg.in/yaml.v3"
)

// hub 直连 v2(签名 URL v0.4.2)在主模块的用例
//
// 覆盖两层契约:
//   - 生成侧(N1/N2): 消息格式与地址形状冻结、门槛矩阵(开关 × 版本 × hub 健康 ×
//     warm 标记)决定签 v2 还是维持 v1;
//   - 换链侧(N4): stale=1 跳过"接受标记"捷径强制重发 /warm(R1 现场自愈),
//     失败即回退 Google 直链, 提示的解析宽松且不破坏既有回退链。
//
// 向量纪律: 期望签名全部由【独立实现】算出(Python3 的 hmac + hashlib)并在用例内
// 再手工拼一次消息与 HMAC 复算 —— 绝不用被测函数生成期望值, 否则实现漂移会被
// 期望值一起带走。
//
//	python3 - <<'PY'
//	import hmac, hashlib
//	key = bytes.fromhex("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
//	msg = "v2\n5b2x6KeG5bqTL-eUteW9sS_mtYvor5Ug5b2x54mHLm1wNA\n1760000000\nhttp://10.0.0.9:8791\n1AbCdEfGhIjKlMnOpQrStUvWxYz"
//	print(hmac.new(key, msg.encode(), hashlib.sha256).hexdigest())
//	PY

const (
	// v2FrozenHubBase 冻结向量里的 hub 内网基址
	v2FrozenHubBase = "http://10.0.0.9:8791"
	// v2FrozenDriveFileID 冻结向量里的 Drive 文件 id
	v2FrozenDriveFileID = "1AbCdEfGhIjKlMnOpQrStUvWxYz"
	// v2FrozenSignature 期望签名(小写 hex, 独立复算见文件头)
	v2FrozenSignature = "89656fb79b23a9053183463831dcb5eae09b88504a49eb4f95d8e7459a302ba1"

	// agentV2FrozenHubBase 与 agent 仓库 sign_v2_test.go 逐字一致的 v2 向量:
	// u 取 v6 形态(顺带证明方括号地址的编码/签名两侧都成立)
	agentV2FrozenHubBase = "http://[2001:db8::1]:8791"
	// agentV2FrozenDriveFileID 同一向量的 Drive 文件 id
	agentV2FrozenDriveFileID = "1HubFile-7"
	// agentV2FrozenSignature 同一向量的期望签名(小写 hex)
	agentV2FrozenSignature = "acd9f0f5ccd4825a2d5e20022325a664d111490fa59707ee82feee23070d520a"
)

func TestSignMessageV2FormatIsFrozen(t *testing.T) {
	got := string(signMessageV2("abc123", "1700000000", "http://10.0.0.9:8791", "drive-1"))
	if got != "v2\nabc123\n1700000000\nhttp://10.0.0.9:8791\ndrive-1" {
		t.Fatalf("签名消息必须逐字为 v2\\n<file_id>\\n<e>\\n<u>\\n<f>, 实际 %q", got)
	}
}

func TestSignClientURLV2_MatchesFrozenVector(t *testing.T) {
	key := masterSignKeyBytes(t)
	rec := &agentRecord{
		ID:            "agent-1",
		SignKey:       masterSignKeyHex,
		PublicBaseURL: "http://node.example.com:8790/",
	}

	url, err := signClientURLV2(rec, masterSignGDPath, v2FrozenHubBase, v2FrozenDriveFileID, time.Unix(1760000000, 0))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	want := "http://node.example.com:8790/dl/" + masterSignFileID + "?e=" + masterSignExpiry +
		"&u=http%3A%2F%2F10.0.0.9%3A8791&f=" + v2FrozenDriveFileID + "&s=" + v2FrozenSignature
	if url != want {
		t.Fatalf("签发的 v2 地址与冻结向量不符:\n实际 %s\n期望 %s", url, want)
	}

	// 独立复算: 测试里自己拼一次消息与 HMAC, 不走 signMessageV2 / signClientURLV2
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("v2\n" + masterSignFileID + "\n" + masterSignExpiry + "\n" + v2FrozenHubBase + "\n" + v2FrozenDriveFileID))
	if got := hex.EncodeToString(mac.Sum(nil)); got != v2FrozenSignature {
		t.Fatalf("独立复算不符: %s != %s", got, v2FrozenSignature)
	}
}

// TestSignMessageV2_MatchesAgentFrozenVector 与 agent 侧的 v2 冻结向量逐字一致
//
// 两侧各自冻结同一份期望值: 任一端的消息格式(前缀、分隔、字段顺序)漂移都会在
// 各自仓库的 CI 里炸, 不会等到联调才发现"sign 对不上"。
func TestSignMessageV2_MatchesAgentFrozenVector(t *testing.T) {
	mac := hmac.New(sha256.New, masterSignKeyBytes(t))
	mac.Write(signMessageV2(agentFrozenFileID, agentFrozenExpiry, agentV2FrozenHubBase, agentV2FrozenDriveFileID))
	if got := hex.EncodeToString(mac.Sum(nil)); got != agentV2FrozenSignature {
		t.Fatalf("与 agent 侧 v2 冻结向量不符: %s != %s", got, agentV2FrozenSignature)
	}
}

func TestSignClientURLV2_Shape(t *testing.T) {
	key := masterSignKeyBytes(t)

	cases := []struct {
		name        string
		rec         *agentRecord
		wantPrefix  string
		hubBase     string
		driveFileID string
	}{
		{
			name:        "v6 节点基址 + v6 hub 基址 + 特殊字符文件 id",
			rec:         &agentRecord{ID: "agent-v6", SignKey: masterSignKeyHex, PublicBaseURL: "http://[2408:8207:1234::5]:8790"},
			wantPrefix:  "http://[2408:8207:1234::5]:8790/dl/",
			hubBase:     "http://[2001:db8::1]:8791",
			driveFileID: "1AbC-d_Ef",
		},
		{
			name:        "按来源 IP 推导 v4 基址",
			rec:         &agentRecord{ID: "agent-v4", SignKey: masterSignKeyHex, LastIP: "10.0.0.1", ListenPort: 9999},
			wantPrefix:  "http://10.0.0.1:9999/dl/",
			hubBase:     v2FrozenHubBase,
			driveFileID: v2FrozenDriveFileID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := signClientURLV2(tc.rec, masterSignGDPath, tc.hubBase, tc.driveFileID, time.Unix(1760000000, 0))
			if err != nil {
				t.Fatalf("签发失败: %v", err)
			}

			// 独立复算: 手工拼消息与 HMAC
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte("v2\n" + masterSignFileID + "\n" + masterSignExpiry + "\n" + tc.hubBase + "\n" + tc.driveFileID))
			wantSig := hex.EncodeToString(mac.Sum(nil))

			// u/f 先取原值算签名, 再按查询参数编码; 参数顺序 e → u → f → s 是协议的一部分
			want := tc.wantPrefix + masterSignFileID + "?e=" + masterSignExpiry +
				"&u=" + url.QueryEscape(tc.hubBase) + "&f=" + url.QueryEscape(tc.driveFileID) + "&s=" + wantSig
			if got != want {
				t.Fatalf("v2 地址形状不符:\n实际 %s\n期望 %s", got, want)
			}
		})
	}

	// 方括号(v6)的转义逐字节冻结: 客户端与 agent 都按这一串消费
	rec := &agentRecord{ID: "agent-v6", SignKey: masterSignKeyHex, PublicBaseURL: "http://[2408:8207:1234::5]:8790"}
	got, err := signClientURLV2(rec, masterSignGDPath, "http://[2001:db8::1]:8791", "1AbC-d_Ef", time.Unix(1760000000, 0))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if !strings.Contains(got, "&u=http%3A%2F%2F%5B2001%3Adb8%3A%3A1%5D%3A8791&f=1AbC-d_Ef&s=") {
		t.Fatalf("v6 hub 基址的查询转义不符: %s", got)
	}
}

func TestSignClientURLV2_Errors(t *testing.T) {
	cases := []struct {
		name        string
		rec         *agentRecord
		hubBase     string
		driveFileID string
	}{
		{"hub 基址为空", &agentRecord{ID: "a", SignKey: masterSignKeyHex, PublicBaseURL: "http://node.example.com"}, "", v2FrozenDriveFileID},
		{"hub 基址只有空白", &agentRecord{ID: "a", SignKey: masterSignKeyHex, PublicBaseURL: "http://node.example.com"}, "   ", v2FrozenDriveFileID},
		{"Drive 文件 id 为空", &agentRecord{ID: "a", SignKey: masterSignKeyHex, PublicBaseURL: "http://node.example.com"}, v2FrozenHubBase, ""},
		{"Drive 文件 id 只有空白", &agentRecord{ID: "a", SignKey: masterSignKeyHex, PublicBaseURL: "http://node.example.com"}, v2FrozenHubBase, "  "},
		{"节点地址不可推导", &agentRecord{ID: "a", SignKey: masterSignKeyHex}, v2FrozenHubBase, v2FrozenDriveFileID},
		{"sign_key 不是 hex", &agentRecord{ID: "a", SignKey: "not-hex!", PublicBaseURL: "http://node.example.com"}, v2FrozenHubBase, v2FrozenDriveFileID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, err := signClientURLV2(tc.rec, masterSignGDPath, tc.hubBase, tc.driveFileID, time.Unix(1760000000, 0))
			if err == nil {
				t.Fatalf("应返回错误, 实际得到地址: %s", url)
			}
			if tc.rec.SignKey != "" && strings.Contains(err.Error(), tc.rec.SignKey) {
				t.Errorf("错误消息回显了 sign_key: %q", err.Error())
			}
		})
	}
}

// TestAgentSupportsHubDirectV2 版本门槛的滚动兼容口径
//
// 关键约定: 解析不出(dev / 空串 / 畸形)一律按"不支持" —— v1 在任何版本上都成立,
// 误判成 v2 才会真的让节点拿不到可用的 URL。
func TestAgentSupportsHubDirectV2(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"0.4.2", true},
		{"v0.4.2", true},
		{"agent-v0.4.2", true},
		{"0.4.2-rc1", true},
		{"v0.4.2-3-gabc1234", true},
		{"0.4.3", true},
		{"0.5.0", true},
		{"0.4.10", true},
		{"0.10.0", true},
		{"1.0.0", true},
		{"0.4.1", false},
		{"0.4.0", false},
		{"0.3.9", false},
		{"", false},
		{"dev", false},
		{"nightly", false},
		{"v0.4", false},
	}

	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			if got := agentSupportsHubDirectV2(tc.version); got != tc.want {
				t.Errorf("agentSupportsHubDirectV2(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// TestStaleHint 陈旧提示的宽松解析: 除空串 / "0" / "false" 外都算提示
//
// 漏读的代价是 R1 要等接受标记自然过期(≤30min), 多读的代价只是多一次同步预热,
// 因此口径刻意宽松 —— 这条表钉住两侧的取舍不被悄悄改掉。
func TestStaleHint(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{" false ", false},
		{"1", true},
		{" 1 ", true},
		{"true", true},
		{"yes", true},
		{"2", true},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := staleHint(tc.raw); got != tc.want {
				t.Errorf("staleHint(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// setupHubDirectV2Config 注入一份可调 hub 直连 v2 开关的完整配置
//
// 走 yaml + Init 的真实解析路径(与 setupHubFullConfig 同款): hub-direct-v2 是
// 新配置键, 必须经 UnmarshalYAML 的显式字段清单落地 —— 直接改结构体字段会掩盖
// "键被静默丢弃"这类问题。hubDirectV2 传 "" 表示缺省(默认开启)。
func setupHubDirectV2Config(t *testing.T, hubPort int, hubEnabled bool, hubDirectV2, apiBase string) {
	t.Helper()

	t.Setenv(config.AgentEnrollTokenEnvName, "")
	t.Setenv(config.GDriveApiTokenEnvName, "")

	doc := fmt.Sprintf("enable: true\nenroll-token: %s\noffline-seconds: 45\nurl-ttl: 1h\nhub-enable: %t\n",
		testEnrollToken, hubEnabled)
	if hubPort > 0 {
		doc += fmt.Sprintf("hub-port: %d\n", hubPort)
	}
	if hubDirectV2 != "" {
		doc += "hub-direct-v2: " + hubDirectV2 + "\n"
	}

	agent := new(config.AgentNetwork)
	if err := yaml.Unmarshal([]byte(doc), agent); err != nil {
		t.Fatalf("解析 agent 网络测试配置失败: %v", err)
	}
	if err := agent.Init(); err != nil {
		t.Fatalf("初始化 agent 网络测试配置失败: %v", err)
	}

	gdriveCfg := &config.GDrive{
		Enable:      apiBase != "",
		ApiBase:     apiBase,
		ApiToken:    testPanelToken,
		MountPrefix: "/home/googleDrive",
	}
	if err := gdriveCfg.Init(); err != nil {
		t.Fatalf("初始化面板测试配置失败: %v", err)
	}

	oldConfig := config.C
	config.C = &config.Config{AgentNetwork: agent, GDrive: gdriveCfg, Ge2o: &config.Ge2o{ApiSecret: testGe2oSecret}}
	t.Cleanup(func() { config.C = oldConfig })

	resetHubWarmStore()
	t.Cleanup(resetHubWarmStore)
}

// assertHubDirectV2URL 校验 v2 地址形状并独立复算签名
func assertHubDirectV2URL(t *testing.T, rawURL, base, token, hubBase, driveFileID string, key []byte) {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("签发的地址无法解析: %v, url=%s", err, rawURL)
	}
	if parsed.Path != "/dl/"+token {
		t.Fatalf("地址路径 = %q, want /dl/%s", parsed.Path, token)
	}
	q := parsed.Query()
	expiry, sig := q.Get("e"), q.Get("s")
	if q.Get("u") != hubBase {
		t.Errorf("u = %q, want %q", q.Get("u"), hubBase)
	}
	if q.Get("f") != driveFileID {
		t.Errorf("f = %q, want %q", q.Get("f"), driveFileID)
	}

	// 独立复算: 手工拼消息与 HMAC(签名针对 u/f 原值)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("v2\n" + token + "\n" + expiry + "\n" + hubBase + "\n" + driveFileID))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if sig != wantSig {
		t.Fatalf("v2 签名无法独立复算:\n实际 %s\n期望 %s", sig, wantSig)
	}

	// 逐字节钉住参数顺序与转义: 客户端与 agent 都按这一串消费
	want := base + "/dl/" + token + "?e=" + expiry +
		"&u=" + url.QueryEscape(hubBase) + "&f=" + url.QueryEscape(driveFileID) + "&s=" + wantSig
	if rawURL != want {
		t.Fatalf("v2 地址与冻结形状不符:\n实际 %s\n期望 %s", rawURL, want)
	}

	expiryUnix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		t.Fatalf("过期时间不是十进制 Unix 秒: %q", expiry)
	}
	// 时效 = 配置的 url-ttl(1h)
	if delta := time.Until(time.Unix(expiryUnix, 0)); delta < 59*time.Minute || delta > 61*time.Minute {
		t.Errorf("签名时效 = %v, want 约 1h", delta)
	}
}

// assertLegacyV1URL 校验 v1 地址维持现状形状(逐字节): e → s 两参, 无 u/f
func assertLegacyV1URL(t *testing.T, rawURL, base, token string, key []byte) {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("签发的地址无法解析: %v, url=%s", err, rawURL)
	}
	if parsed.Path != "/dl/"+token {
		t.Fatalf("地址路径 = %q, want /dl/%s", parsed.Path, token)
	}
	q := parsed.Query()
	if _, has := q["u"]; has {
		t.Errorf("v1 地址不得携带 u 参数: %s", rawURL)
	}
	if _, has := q["f"]; has {
		t.Errorf("v1 地址不得携带 f 参数: %s", rawURL)
	}

	expiry, sig := q.Get("e"), q.Get("s")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("v1\n" + token + "\n" + expiry))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if sig != wantSig {
		t.Fatalf("v1 签名无法独立复算:\n实际 %s\n期望 %s", sig, wantSig)
	}
	if want := base + "/dl/" + token + "?e=" + expiry + "&s=" + wantSig; rawURL != want {
		t.Fatalf("v1 地址与现状形状不符(逐字节):\n实际 %s\n期望 %s", rawURL, want)
	}
}

// hubDirectGateCase 门槛矩阵的一格
//
// 五个维度: 配置开关(hub-direct-v2 / hub-enable) × 节点版本 ≥/<0.4.2 × hub 健康
// 与否 × warm 接受标记(有 / 无 / 属于另一台 hub / 指向已失效的 hub)。
type hubDirectGateCase struct {
	name string
	// hubDirect "hub-direct-v2" 的配置原文("" = 缺省)
	hubDirect string
	// hubRouting hub-enable 开关
	hubRouting bool
	// nodeVersion 节点心跳自报版本
	nodeVersion string
	// hubSeeded 是否注册一台 hub 记录
	hubSeeded bool
	// hubFresh hub 心跳是否新鲜(新鲜 = 可被选点)
	hubFresh bool
	// warm 是否先经生产路径 WarmFile 让"已接受"标记生效
	warm bool
	// markAcceptedFor 直接登记接受标记的 hub id("" = 不登记)
	markAcceptedFor string
	// wantV2 期望本次签发 v2
	wantV2 bool
}

// TestPickAndSign_HubDirectV2Gate 307 签发的门槛矩阵: 缺一即维持 v1(现行为)
func TestPickAndSign_HubDirectV2Gate(t *testing.T) {
	cases := []hubDirectGateCase{
		{name: "门槛全满足(缺省开关)", hubRouting: true, nodeVersion: "0.4.2", hubSeeded: true, hubFresh: true, warm: true, wantV2: true},
		{name: "版本带 v 前缀同样满足", hubRouting: true, nodeVersion: "v0.4.2", hubSeeded: true, hubFresh: true, warm: true, wantV2: true},
		{name: "补丁号更大(0.4.10)视为支持", hubRouting: true, nodeVersion: "0.4.10", hubSeeded: true, hubFresh: true, warm: true, wantV2: true},
		{name: "开关显式关闭", hubDirect: "false", hubRouting: true, nodeVersion: "0.4.2", hubSeeded: true, hubFresh: true, warm: true, wantV2: false},
		{name: "节点版本过旧", hubRouting: true, nodeVersion: "0.4.1", hubSeeded: true, hubFresh: true, warm: true, wantV2: false},
		{name: "版本不可解析", hubRouting: true, nodeVersion: "dev", hubSeeded: true, hubFresh: true, warm: true, wantV2: false},
		{name: "warm 未被接受", hubRouting: true, nodeVersion: "0.4.2", hubSeeded: true, hubFresh: true, wantV2: false},
		{name: "标记属于另一台 hub", hubRouting: true, nodeVersion: "0.4.2", hubSeeded: true, hubFresh: true, markAcceptedFor: "hub-other", wantV2: false},
		{name: "hub 心跳过期", hubRouting: true, nodeVersion: "0.4.2", hubSeeded: true, hubFresh: false, markAcceptedFor: "hub-1", wantV2: false},
		{name: "没有 hub 记录", hubRouting: true, nodeVersion: "0.4.2", hubSeeded: false, wantV2: false},
		{name: "hub 接入关闭", hubRouting: false, nodeVersion: "0.4.2", hubSeeded: true, hubFresh: true, markAcceptedFor: "hub-1", wantV2: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := newFakeHub(t, http.StatusOK)
			fileID := uniqueDriveFileID()
			gdPath := uniqueHubGDPath("门槛")
			panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID),
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "", "")
			setupStateDir(t)
			setupHubDirectV2Config(t, hub.port, tc.hubRouting, tc.hubDirect, panel.URL)
			simulateRestart()

			if tc.hubSeeded {
				ago := time.Duration(0)
				if !tc.hubFresh {
					// 超过 offline-seconds(45s): 不再健康
					ago = 46 * time.Second
				}
				seedHub(t, "hub-1", "127.0.0.1", 0, ago)
			}
			seedRecord(t, &agentRecord{
				MachineID: "m-gate", Name: "m-gate", Role: RoleNode, Version: tc.nodeVersion,
				LastIP: "127.0.0.1", ListenPort: 8790, Enabled: true, LastSeenAt: time.Now(),
			})

			wantHubCalls := int64(0)
			if tc.warm {
				accepted, err := WarmFile(context.Background(), gdPath)
				if err != nil || !accepted {
					t.Fatalf("预热应被 hub 接受: accepted=%v, err=%v", accepted, err)
				}
				wantHubCalls = 1
			}
			if tc.markAcceptedFor != "" {
				hubWarmStore.markAccepted(fileID, tc.markAcceptedFor, time.Now())
			}

			collector := &collectedLogs{}
			logID, ok := logs.RegisterLogger(collector)
			if !ok {
				t.Fatal("注册日志收集器失败")
			}
			defer logs.RemoveLogger(logID)

			signed, err := PickAndSign(gdPath)
			if err != nil {
				t.Fatalf("调度失败: %v", err)
			}

			// 签发本身不得联系 hub: 预热只发生在浏览与换链路径
			if got := hub.calls.Load(); got != wantHubCalls {
				t.Errorf("PickAndSign 不应触发 /warm: 请求数 = %d, want %d", got, wantHubCalls)
			}

			nodeBase := "http://127.0.0.1:8790"
			hubBase := "http://127.0.0.1:" + strconv.Itoa(hub.port)
			if tc.wantV2 {
				assertHubDirectV2URL(t, signed, nodeBase, fileToken(gdPath), hubBase, fileID, masterSignKeyBytes(t))
				if !strings.Contains(collector.String(), "上游: hub 直连") {
					t.Errorf("v2 签发应留下可运维的链路日志: %q", collector.String())
				}
				return
			}

			assertLegacyV1URL(t, signed, nodeBase, fileToken(gdPath), masterSignKeyBytes(t))
			if strings.Contains(collector.String(), "hub 直连") {
				t.Errorf("维持 v1 的签发不得出现 hub 直连日志: %q", collector.String())
			}
		})
	}
}

// ============================ stale=1: 现场重预热(N4 / R1 自愈) ============================

// staleFixture 换链接口用例的公共舞台
type staleFixture struct {
	// hub 假 hub(控制面 /warm)
	hub *fakeHub
	// engine master 测试引擎
	engine http.Handler
	// auth 节点凭据请求头
	auth map[string]string
	// gdPath Drive 路径
	gdPath string
	// fileID Drive 文件 id(从直链里解析出来的那个)
	fileID string
	// hubURL hub 数据面地址(上游改写的期望值)
	hubURL string
	// expiresAt 面板下发的过期时间(必须原样透传)
	expiresAt string
	// logBuf 模块日志收集器
	logBuf *collectedLogs
}

// newStaleFixture 搭一台 hub + 一个节点 + 假面板的最小舞台
func newStaleFixture(t *testing.T, hubStatus int) *staleFixture {
	t.Helper()

	hub := newFakeHub(t, hubStatus)
	fileID := uniqueDriveFileID()
	gdPath := uniqueHubGDPath("陈旧换链")
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), expiresAt, "", "")
	setupStateDir(t)
	setupHubDirectV2Config(t, hub.port, true, "", panel.URL)
	simulateRestart()

	collector := &collectedLogs{}
	logID, ok := logs.RegisterLogger(collector)
	if !ok {
		t.Fatal("注册日志收集器失败")
	}
	t.Cleanup(func() { logs.RemoveLogger(logID) })

	engine := newTestEngine()
	node := enrollAgent(t, engine, "m-node")
	hubAgent := enrollHub(t, engine, "m-hub")
	heartbeatFrom(t, engine, hubAgent["agent_id"].(string), hubAgent["agent_secret"].(string), "127.0.0.1")

	return &staleFixture{
		hub:       hub,
		engine:    engine,
		auth:      agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string)),
		gdPath:    gdPath,
		fileID:    fileID,
		hubURL:    "http://127.0.0.1:" + strconv.Itoa(hub.port) + "/f/" + fileID,
		expiresAt: expiresAt,
		logBuf:    collector,
	}
}

// request 请求一次下载直链, staleSuffix 追加在标准查询串之后("" / "&stale=1" / "&stale=0")
func (f *staleFixture) request(t *testing.T, staleSuffix string) map[string]any {
	t.Helper()

	recorder := doRequest(t, f.engine, http.MethodGet, downloadLinkURL(f.gdPath)+staleSuffix, "", f.auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("下发直链应成功: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
	return decodeMap(t, recorder.Body.Bytes())
}

// assertHubUpstream 断言响应指向 hub 数据面: 地址改写 + headers 为空 + 过期时间透传
func (f *staleFixture) assertHubUpstream(t *testing.T, resp map[string]any) {
	t.Helper()

	if resp["url"] != f.hubURL {
		t.Errorf("上游 = %v, want %v", resp["url"], f.hubURL)
	}
	headers, ok := resp["headers"].(map[string]any)
	if !ok || len(headers) != 0 {
		t.Errorf("hub 上游不得携带凭据(headers 应为空): %v", resp["headers"])
	}
	if resp["expires_at"] != f.expiresAt {
		t.Errorf("expires_at 必须原样透传: %v, want %s", resp["expires_at"], f.expiresAt)
	}
}

// assertGoogleUpstream 断言响应是 Google 直链回退(直链 + 凭据 + 过期时间)
func (f *staleFixture) assertGoogleUpstream(t *testing.T, resp map[string]any) {
	t.Helper()

	if resp["url"] != driveDirectLink(f.fileID) {
		t.Errorf("回退上游 = %v, want Google 直链 %v", resp["url"], driveDirectLink(f.fileID))
	}
	headers, ok := resp["headers"].(map[string]any)
	if !ok || headers["Authorization"] != testGoogleHeader {
		t.Errorf("回退时必须带上 Google 凭据, 实际: %v", resp["headers"])
	}
	if resp["expires_at"] != f.expiresAt {
		t.Errorf("expires_at 必须原样透传: %v, want %s", resp["expires_at"], f.expiresAt)
	}
}

// TestDownloadLink_StaleForcesRewarmEvenWhenAccepted N4/R1: stale 提示跳过标记捷径
func TestDownloadLink_StaleForcesRewarmEvenWhenAccepted(t *testing.T) {
	f := newStaleFixture(t, http.StatusOK)

	// 第一次(无提示): 标记不存在 → 同步补发预热, 上游改写为 hub
	resp := f.request(t, "")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 1 {
		t.Fatalf("首次请求应同步补发 1 次 warm, 实际 %d", got)
	}

	// 第二次: 标记命中 → 稳态零补发(纯路由信息, 不再回访的必要)
	resp = f.request(t, "")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 1 {
		t.Errorf("标记有效时不应重复补发, 实际 %d 次", got)
	}

	// stale=0 等价"没有提示": 不得触发重预热(宽松解析的否定侧)
	resp = f.request(t, "&stale=0")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 1 {
		t.Errorf("stale=0 不得触发重预热, 实际 %d 次", got)
	}

	// stale=1: 即便标记仍然有效, 也必须强制重发一次 /warm(hub 重启丢内存的现场自愈)
	resp = f.request(t, "&stale=1")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 2 {
		t.Fatalf("stale=1 必须强制重发 1 次 warm, 实际 %d 次", got)
	}
	if got := strings.Count(f.logBuf.String(), "hub 陈旧换链: 已重新预热"); got != 1 {
		t.Errorf("陈旧换链成功应留下 1 条日志, 实际 %d 条: %q", got, f.logBuf.String())
	}

	// 重预热再次登记标记: 后续无提示请求回到稳态
	resp = f.request(t, "")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 2 {
		t.Errorf("重预热后标记应再次生效, 实际 %d 次", got)
	}

	// 日志卫生: 面板令牌、Google 凭据与完整直链都不得进日志
	logged := f.logBuf.String()
	for name, secretValue := range map[string]string{
		"面板令牌":      testPanelToken,
		"Google 凭据": testGoogleHeader,
		"完整直链":      driveDirectLink(f.fileID),
	} {
		if strings.Contains(logged, secretValue) {
			t.Errorf("日志里出现了 %s: %q", name, logged)
		}
	}
}

// TestDownloadLink_StaleRewarmFailureFallsBackToGoogle R1 的停机侧: 重预热失败即回退
func TestDownloadLink_StaleRewarmFailureFallsBackToGoogle(t *testing.T) {
	f := newStaleFixture(t, http.StatusOK)

	// 前置: 正常预热一次, 标记生效(上游此时指向 hub)
	resp := f.request(t, "")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 1 {
		t.Fatalf("前置预热应恰好 1 次, 实际 %d", got)
	}

	// hub 转为故障(重启窗口 / 进程死亡带来的 5xx): stale=1 的重预热必须失败并回退
	f.hub.status.Store(http.StatusInternalServerError)
	resp = f.request(t, "&stale=1")
	f.assertGoogleUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 2 {
		t.Fatalf("stale=1 应重试一次 warm(失败), 实际 %d 次", got)
	}
	if !strings.Contains(f.logBuf.String(), "hub 预热未被接受, 本次直连 Google") {
		t.Errorf("重预热失败回退应留下日志: %q", f.logBuf.String())
	}
	if strings.Contains(f.logBuf.String(), "hub 陈旧换链: 已重新预热") {
		t.Errorf("预热失败不得记录换链成功: %q", f.logBuf.String())
	}

	// 冷却窗口内再次 stale: 不再等待 hub, 立刻回退(回退链是硬要求)
	resp = f.request(t, "&stale=1")
	f.assertGoogleUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 2 {
		t.Errorf("冷却窗口内不得再打扰 hub, 实际 %d 次", got)
	}

	// hub 恢复: 冷却结束后 stale=1 重预热成功, 上游回到 hub(停机自愈闭环)
	resetHubWarmStore()
	f.hub.status.Store(http.StatusOK)
	resp = f.request(t, "&stale=1")
	f.assertHubUpstream(t, resp)
	if got := f.hub.calls.Load(); got != 3 {
		t.Errorf("hub 恢复后应成功重预热, 实际 %d 次", got)
	}
	if got := strings.Count(f.logBuf.String(), "hub 陈旧换链: 已重新预热"); got != 1 {
		t.Errorf("恢复后应记录 1 次换链成功, 实际 %d 条: %q", got, f.logBuf.String())
	}
}

// TestDownloadLink_StaleWithoutHealthyHub 没有健康 hub 时 stale 提示不得改变回退落点
func TestDownloadLink_StaleWithoutHealthyHub(t *testing.T) {
	hub := newFakeHub(t, http.StatusOK) // 只监听, 期望全程不被联系
	fileID := uniqueDriveFileID()
	gdPath := uniqueHubGDPath("陈旧换链无hub")
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	panel := newFakePanel(t, http.StatusOK, driveDirectLink(fileID), expiresAt, "", "")
	setupStateDir(t)
	setupHubDirectV2Config(t, hub.port, true, "", panel.URL)
	simulateRestart()

	engine := newTestEngine()
	node := enrollAgent(t, engine, "m-node")

	recorder := doRequest(t, engine, http.MethodGet, downloadLinkURL(gdPath)+"&stale=1", "",
		agentAuthHeaders(node["agent_id"].(string), node["agent_secret"].(string)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("回退路径必须仍然 200: HTTP %d, body=%s", recorder.Code, recorder.Body.String())
	}
	// 回退响应与"未部署 hub"逐字节一致(现状形状)
	assertGoogleDirectResponse(t, recorder.Body.Bytes(), driveDirectLink(fileID), expiresAt)
	if got := hub.calls.Load(); got != 0 {
		t.Errorf("没有健康 hub 时不得联系 hub, 实际 %d 次", got)
	}
}
