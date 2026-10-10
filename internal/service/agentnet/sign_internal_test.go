package agentnet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"

	"gopkg.in/yaml.v3"
)

// 冻结的签名向量
//
// 两条向量的期望值都由【独立实现】算出(Python3 的 hmac + hashlib),
// 而不是"用本仓库的代码算一遍再抄过来", 这样才能发现本仓库实现漂移:
//
//	python3 - <<'PY'
//	import base64, hmac, hashlib
//	key = bytes.fromhex("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
//	gd_path = "影视库/电影/测试 影片.mp4"
//	token = base64.urlsafe_b64encode(gd_path.encode()).rstrip(b"=").decode()
//	msg = f"v1\n{token}\n1760000000"
//	print(token, hmac.new(key, msg.encode(), hashlib.sha256).hexdigest())
//	PY
//
// 其中 agent 侧向量(masterSignKeyHex + agentFrozenFileID)还与子任务 C 的
// agent 仓库测试(agent/internal/proxy/sign_test.go)逐字一致:
// 两侧各自冻结同一份期望值, 任一实现漂移都会在各自仓库的 CI 里炸。
const (
	// masterSignKeyHex 测试用签名密钥(32 字节 hex)
	masterSignKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	// masterSignGDPath 测试用 Drive 路径(含中文与空格, 顺带证明 base64url 编码正确)
	masterSignGDPath = "影视库/电影/测试 影片.mp4"
	// masterSignFileID base64.RawURLEncoding(masterSignGDPath)
	masterSignFileID = "5b2x6KeG5bqTL-eUteW9sS_mtYvor5Ug5b2x54mHLm1wNA"
	// masterSignExpiry 测试用过期时间(Unix 秒)
	masterSignExpiry = "1760000000"
	// masterSignSignature 期望签名(小写 hex)
	masterSignSignature = "6e69aa96e5e4bb57867e87ea75210a134d2bdffddb473a4dd54aedf6b33c39e0"

	// agentFrozenFileID 与 agent 侧冻结向量相同的 file_id(token 即 agent 眼中的 file_id)
	agentFrozenFileID = "1AbCdEfGhIjKlMnOpQrStUvWxYz"
	// agentFrozenExpiry 与 agent 侧冻结向量相同的过期时间
	agentFrozenExpiry = "1760000000"
	// agentFrozenSignature 与 agent 侧冻结向量相同的期望签名
	agentFrozenSignature = "9f13437c21616fe83d89c99adff47e5530dfe113788d000c5b9710e4af52b293"
)

// masterSignKeyBytes 解码测试密钥
func masterSignKeyBytes(t *testing.T) []byte {
	t.Helper()
	key, err := hex.DecodeString(masterSignKeyHex)
	if err != nil {
		t.Fatalf("测试密钥不是合法 hex: %v", err)
	}
	return key
}

func TestSignMessageFormatIsFrozen(t *testing.T) {
	if got := string(signMessage("abc123", "1700000000")); got != "v1\nabc123\n1700000000" {
		t.Fatalf("签名消息必须逐字为 v1\\n<file_id>\\n<e>, 实际 %q", got)
	}
}

func TestFileTokenRoundTrip(t *testing.T) {
	token := fileToken(masterSignGDPath)
	if token != masterSignFileID {
		t.Errorf("file token = %q, want %q", token, masterSignFileID)
	}
	if strings.ContainsAny(token, "+/=") {
		t.Errorf("file token 必须是无填充 base64url(不含 + / =): %q", token)
	}

	got, err := decodeFileToken(token)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if got != masterSignGDPath {
		t.Errorf("解码结果 = %q, want %q", got, masterSignGDPath)
	}
}

func TestDecodeFileToken_Errors(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"含非法字符", "abc+123"},
		{"含标准 base64 填充", "YWJj="},
		{"空串", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeFileToken(tc.token); err == nil {
				t.Fatal("非法 file_id 应解码失败")
			}
		})
	}

	// 能解码但解出来是空路径: 同样不接受
	if _, err := decodeFileToken(base64.RawURLEncoding.EncodeToString([]byte("   "))); err == nil {
		t.Fatal("解码后为空白的 file_id 应被拒绝")
	}
}

func TestSignClientURL_MatchesFrozenVectors(t *testing.T) {
	key := masterSignKeyBytes(t)
	rec := &agentRecord{
		ID:            "agent-1",
		SignKey:       masterSignKeyHex,
		PublicBaseURL: "http://node.example.com:8790/",
	}

	expiresAt := time.Unix(1760000000, 0)
	url, err := signClientURL(rec, masterSignGDPath, expiresAt)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	want := "http://node.example.com:8790/dl/" + masterSignFileID + "?e=" + masterSignExpiry + "&s=" + masterSignSignature
	if url != want {
		t.Fatalf("签发的地址与冻结向量不符:\n实际 %s\n期望 %s", url, want)
	}

	// 独立复算: 测试里自己拼一次消息与 HMAC, 不走 signMessage 的实现路径
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("v1" + "\n" + masterSignFileID + "\n" + masterSignExpiry))
	if got := hex.EncodeToString(mac.Sum(nil)); got != masterSignSignature {
		t.Fatalf("独立复算不符: %s != %s", got, masterSignSignature)
	}
}

func TestSignClientURL_MatchesAgentFrozenVector(t *testing.T) {
	// 与 agent 侧冻结向量逐字一致: 消息里的 file_id 直接就是 token
	key := masterSignKeyBytes(t)
	mac := hmac.New(sha256.New, key)
	mac.Write(signMessage(agentFrozenFileID, agentFrozenExpiry))
	if got := hex.EncodeToString(mac.Sum(nil)); got != agentFrozenSignature {
		t.Fatalf("与 agent 侧冻结向量不符: %s != %s", got, agentFrozenSignature)
	}
}

func TestSignClientURL_AddressDerivation(t *testing.T) {
	cases := []struct {
		name    string
		rec     *agentRecord
		wantPre string
		wantErr bool
	}{
		{
			"优先使用 public_base_url",
			&agentRecord{ID: "a", SignKey: masterSignKeyHex, PublicBaseURL: "https://node.example.com", LastIP: "10.0.0.1", ListenPort: 9999},
			"https://node.example.com/dl/",
			false,
		},
		{
			"按来源 IP 推导",
			&agentRecord{ID: "a", SignKey: masterSignKeyHex, LastIP: "10.0.0.1", ListenPort: 9999},
			"http://10.0.0.1:9999/dl/",
			false,
		},
		{
			"IPv6 来源地址需要方括号",
			&agentRecord{ID: "a", SignKey: masterSignKeyHex, LastIP: "2001:db8::1", ListenPort: 8790},
			"http://[2001:db8::1]:8790/dl/",
			false,
		},
		{
			"端口缺失时用默认端口",
			&agentRecord{ID: "a", SignKey: masterSignKeyHex, LastIP: "10.0.0.1"},
			"http://10.0.0.1:8790/dl/",
			false,
		},
		{
			"地址不可推导",
			&agentRecord{ID: "a", SignKey: masterSignKeyHex},
			"",
			true,
		},
		{
			"sign_key 不是 hex",
			&agentRecord{ID: "a", SignKey: "not-hex!", LastIP: "10.0.0.1"},
			"",
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, err := signClientURL(tc.rec, masterSignGDPath, time.Unix(1760000000, 0))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应返回错误, 实际得到 %s", url)
				}
				if strings.Contains(err.Error(), tc.rec.SignKey) && tc.rec.SignKey != "" {
					t.Errorf("错误消息回显了 sign_key: %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("签发失败: %v", err)
			}
			if !strings.HasPrefix(url, tc.wantPre) {
				t.Errorf("地址前缀 = %q, want %q", url, tc.wantPre)
			}
		})
	}
}

// v6 基址的签名全链: 地址组装(方括号 + 端口)与签名消息在 v6 下同样成立
//
// A1: 签名只取决于 file_id / e / sign_key, 与基址无关——这里刻意断言与 v4
// 冻结向量**逐字相同**的签名, 用来证明"换 v6 基址没有动签名计算"。
func TestSignClientURL_IPv6Base(t *testing.T) {
	cases := []struct {
		name string
		rec  *agentRecord
		want string
	}{
		{
			"按 v6 来源 IP 推导(JoinHostPort 补方括号)",
			&agentRecord{ID: "agent-v6", SignKey: masterSignKeyHex, LastIP: "2001:db8::1", ListenPort: 8790},
			"http://[2001:db8::1]:8790/dl/" + masterSignFileID + "?e=" + masterSignExpiry + "&s=" + masterSignSignature,
		},
		{
			"v6 public_base_url 原样使用(带端口)",
			&agentRecord{ID: "agent-v6", SignKey: masterSignKeyHex, PublicBaseURL: "http://[2408:8207:1234::5]:8790"},
			"http://[2408:8207:1234::5]:8790/dl/" + masterSignFileID + "?e=" + masterSignExpiry + "&s=" + masterSignSignature,
		},
		{
			"v6 public_base_url 无端口",
			&agentRecord{ID: "agent-v6", SignKey: masterSignKeyHex, PublicBaseURL: "https://[2001:db8::1]"},
			"https://[2001:db8::1]/dl/" + masterSignFileID + "?e=" + masterSignExpiry + "&s=" + masterSignSignature,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, err := signClientURL(tc.rec, masterSignGDPath, time.Unix(1760000000, 0))
			if err != nil {
				t.Fatalf("签发失败: %v", err)
			}
			if url != tc.want {
				t.Fatalf("v6 基址签发的地址不符:\n实际 %s\n期望 %s", url, tc.want)
			}
			// 独立复算: 不走 signMessage, 直接拼消息与 HMAC
			mac := hmac.New(sha256.New, masterSignKeyBytes(t))
			mac.Write([]byte("v1\n" + masterSignFileID + "\n" + masterSignExpiry))
			if got := hex.EncodeToString(mac.Sum(nil)); got != masterSignSignature {
				t.Fatalf("独立复算不符: %s != %s", got, masterSignSignature)
			}
		})
	}
}

// 心跳上报 v6 地址 → 调度 → 签发: 端到端全链在 v6 基址下可用
func TestPickAndSign_IPv6Addresses(t *testing.T) {
	cases := []struct {
		name          string
		lastIP        string
		publicBaseURL string
		wantPrefix    string
	}{
		{
			name:       "v6 来源 IP",
			lastIP:     "2001:db8::9",
			wantPrefix: "http://[2001:db8::9]:8790/dl/",
		},
		{
			name:          "v6 public_base_url",
			lastIP:        "10.0.0.9",
			publicBaseURL: "http://[2408:8207:1234::5]:8790",
			wantPrefix:    "http://[2408:8207:1234::5]:8790/dl/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupStateDir(t)
			setupAgentConfig(t, true)

			result, err := defaultRegistry.enroll(testEnrollParams("machine-v6"))
			if err != nil {
				t.Fatalf("注册失败: %v", err)
			}
			if _, err := defaultRegistry.touch(result.AgentID, heartbeatParams{
				LastIP:        tc.lastIP,
				PublicBaseURL: tc.publicBaseURL,
				Now:           time.Now(),
			}); err != nil {
				t.Fatalf("心跳失败: %v", err)
			}

			url, err := PickAndSign(masterSignGDPath)
			if err != nil {
				t.Fatalf("调度失败: %v", err)
			}
			prefix := tc.wantPrefix + fileToken(masterSignGDPath) + "?e="
			if !strings.HasPrefix(url, prefix) {
				t.Fatalf("地址形状不符: %s", url)
			}

			// 从 URL 里取出 e 与 s, 用节点当前密钥独立复算
			query := strings.TrimPrefix(url, prefix)
			expiry, sign, ok := strings.Cut(query, "&s=")
			if !ok {
				t.Fatalf("地址缺少签名参数: %s", url)
			}
			list, err := defaultRegistry.snapshot()
			if err != nil {
				t.Fatalf("读取注册表失败: %v", err)
			}
			key, err := hex.DecodeString(list[0].SignKey)
			if err != nil {
				t.Fatalf("节点 sign_key 不是合法 hex: %v", err)
			}
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte("v1\n" + fileToken(masterSignGDPath) + "\n" + expiry))
			if want := hex.EncodeToString(mac.Sum(nil)); sign != want {
				t.Fatalf("v6 链路的签名无法独立复算:\n实际 %s\n期望 %s", sign, want)
			}
		})
	}
}

func TestPickAndSign(t *testing.T) {
	t.Run("未启用", func(t *testing.T) {
		setupStateDir(t)
		setupAgentConfig(t, false)

		if _, err := PickAndSign(masterSignGDPath); !errors.Is(err, ErrDisabled) {
			t.Fatalf("未启用时应返回 ErrDisabled, 实际: %v", err)
		}
	})

	t.Run("没有可用节点", func(t *testing.T) {
		setupStateDir(t)
		setupAgentConfig(t, true)
		simulateRestart()

		if _, err := PickAndSign(masterSignGDPath); !errors.Is(err, ErrNoAgent) {
			t.Fatalf("没有节点时应返回 ErrNoAgent, 实际: %v", err)
		}
	})

	t.Run("内部故障不算没有节点", func(t *testing.T) {
		// 注册表损坏时返回的是内部错误, 不能被误判成 ErrNoAgent,
		// 否则调用方会按"没有节点"回退或 503, 掩盖真正的问题
		basePath := setupStateDir(t)
		setupAgentConfig(t, true)
		simulateRestart()
		if err := os.MkdirAll(filepath.Join(basePath, DirName), 0o755); err != nil {
			t.Fatalf("创建状态目录失败: %v", err)
		}
		if err := os.WriteFile(agentsFilePath(basePath), []byte("{损坏"), 0o600); err != nil {
			t.Fatalf("写入损坏注册表失败: %v", err)
		}

		_, err := PickAndSign(masterSignGDPath)
		if err == nil || errors.Is(err, ErrNoAgent) || errors.Is(err, ErrDisabled) {
			t.Fatalf("内部故障不应等同于 ErrNoAgent / ErrDisabled, 实际: %v", err)
		}
	})

	t.Run("调度成功且签名可独立复算", func(t *testing.T) {
		setupStateDir(t)
		setupAgentConfig(t, true)

		result, err := defaultRegistry.enroll(testEnrollParams("machine-1"))
		if err != nil {
			t.Fatalf("注册失败: %v", err)
		}
		if _, err := defaultRegistry.touch(result.AgentID, heartbeatParams{
			LastIP: "192.168.1.10", Now: time.Now(),
		}); err != nil {
			t.Fatalf("心跳失败: %v", err)
		}

		url, err := PickAndSign(masterSignGDPath)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}

		prefix := "http://192.168.1.10:8790/dl/" + fileToken(masterSignGDPath) + "?e="
		if !strings.HasPrefix(url, prefix) {
			t.Fatalf("地址形状不符: %s", url)
		}

		// 从 URL 里取出 e 与 s, 用节点当前密钥独立复算
		query := strings.TrimPrefix(url, prefix)
		expiry, sign, ok := strings.Cut(query, "&s=")
		if !ok {
			t.Fatalf("地址缺少签名参数: %s", url)
		}
		expiryUnix, err := strconv.ParseInt(expiry, 10, 64)
		if err != nil {
			t.Fatalf("过期时间不是十进制 Unix 秒: %q", expiry)
		}
		// 时效 = 配置的 url-ttl(24h)
		if delta := time.Until(time.Unix(expiryUnix, 0)); delta < 23*time.Hour || delta > 25*time.Hour {
			t.Errorf("签名时效 = %v, want 约 24h", delta)
		}

		list, err := defaultRegistry.snapshot()
		if err != nil {
			t.Fatalf("读取注册表失败: %v", err)
		}
		key, err := hex.DecodeString(list[0].SignKey)
		if err != nil {
			t.Fatalf("节点 sign_key 不是合法 hex: %v", err)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("v1\n" + fileToken(masterSignGDPath) + "\n" + expiry))
		if want := hex.EncodeToString(mac.Sum(nil)); sign != want {
			t.Fatalf("节点私自篡改了签名:\n实际 %s\n期望 %s", sign, want)
		}
	})

	t.Run("priority 策略经配置接线到选点", func(t *testing.T) {
		// schedule-strategy 是 config 的私有字段, 只能像生产一样经 yaml 解析 + Init 填值。
		// 本用例钉住 PickAndSign → schedule 的策略接线: 参数传错时选点会退回 least-active。
		setupStateDir(t)
		var parsed config.Config
		raw := "agent-network:\n" +
			"  enable: true\n" +
			"  enroll-token: test-enroll-token-0123456789\n" +
			"  schedule-strategy: priority\n"
		if err := yaml.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("解析 yaml 失败: %v", err)
		}
		if parsed.AgentNetwork == nil {
			t.Fatal("yaml 解析后 agent-network 段为空")
		}
		if err := parsed.AgentNetwork.Init(); err != nil {
			t.Fatalf("初始化配置失败: %v", err)
		}
		oldCfg := config.C
		config.C = &config.Config{AgentNetwork: parsed.AgentNetwork}
		t.Cleanup(func() { config.C = oldCfg })
		simulateRestart()

		now := time.Now()
		// 高优先级节点很忙, 低优先级节点空闲: priority 策略必须忽略活跃流选中前者
		seedRecord(t, &agentRecord{
			MachineID: "busy-top", Name: "busy-top", LastIP: "10.0.0.1", ListenPort: 8790,
			Enabled: true, LastSeenAt: now, ActiveStreams: 9,
		})
		seedRecord(t, &agentRecord{
			MachineID: "idle-low", Name: "idle-low", LastIP: "10.0.0.2", ListenPort: 8790,
			Enabled: true, LastSeenAt: now, ActiveStreams: 0, Priority: 5,
		})

		url, err := PickAndSign(masterSignGDPath)
		if err != nil {
			t.Fatalf("调度失败: %v", err)
		}
		if !strings.HasPrefix(url, "http://10.0.0.1:8790/dl/") {
			t.Fatalf("配置的 priority 策略未接线到选点(应调度到高优先级节点 busy-top), 实际: %s", url)
		}
	})
}
