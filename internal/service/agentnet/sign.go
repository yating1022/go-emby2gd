package agentnet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/config"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs/colors"
)

// signMessage 客户端 URL 的签名消息
//
// 逐字为 "v1\n<file_id>\n<e>": file_id 是 base64url(无填充) 编码的 Drive 路径,
// e 是十进制的 Unix 秒级过期时间。格式一经冻结即协议, agent 侧的验签
// 按同一份消息重算; 改动这里等于破坏协议(两侧都要改).
func signMessage(fileID, expiry string) []byte {
	return []byte("v1\n" + fileID + "\n" + expiry)
}

// signMessageV2 v2(hub 直连)客户端 URL 的签名消息(v0.4.2, 任务 10-10-agent-hub-direct-v2)
//
// 逐字为 "v2\n<file_id>\n<e>\n<u>\n<f>": 在 v1 的基础上多签了
// u(hub 内网基址, 如 http://10.0.0.9:8791)与 f(Google Drive 文件 id)。
// 两者决定"这次请求去哪里取哪个文件", **必须进签名**——否则一条合法 URL
// 会被改造成"任意文件代理"(N1 的安全红线)。
//
// u/f 用的是**原值**: 这里算 HMAC, 签完再由 signClientURLV2 按查询参数编码;
// agent 侧解码后按同一份原值复算, 两端不能有任何编码差异。
func signMessageV2(fileID, expiry, hubBase, driveFileID string) []byte {
	return []byte("v2\n" + fileID + "\n" + expiry + "\n" + hubBase + "\n" + driveFileID)
}

// agentHubDirectMinVersion 支持 v2 hub 直连 URL 的最低 agent 版本
//
// v0.4.2 起 agent 才认得 u/f 参数: 更早的版本只校验 v1 消息, URL 上多出来的
// 查询参数不影响 v1 验签(file_id 与 e 不变)——但签出去的 v2 地址对它们而言
// 无法使用, 因此发布节奏是"master 先行, 只对 ≥0.4.2 的节点签 v2"(N2/N6)。
var agentHubDirectMinVersion = [3]int{0, 4, 2}

// agentSupportsHubDirectV2 判定节点自报版本是否 ≥ 0.4.2
//
// 版本串由发布流水线注入(-ldflags "-X main.version=<tag>", tag 形如
// agent-v0.4.2 → 去掉前缀后是 "0.4.2"; 也容忍 "v0.4.2" 这类写法)。
// 判定规则: 取第一个 major.minor.patch 三元组比较; 解析不出(dev / 空串 /
// 畸形)一律按"不支持"处理 —— 滚动兼容期保守优先: v1 在任何版本上都成立,
// 误判成 v2 才会真的让节点拿不到可用的 URL。
func agentSupportsHubDirectV2(version string) bool {
	major, minor, patch, ok := parseVersionTriple(version)
	if !ok {
		return false
	}
	cur := [3]int{major, minor, patch}
	for i := 0; i < 3; i++ {
		if cur[i] != agentHubDirectMinVersion[i] {
			return cur[i] > agentHubDirectMinVersion[i]
		}
	}
	return true
}

// parseVersionTriple 从版本串里解析出第一组 major.minor.patch
//
// 不用正则: 只扫第一个数字段及其后的两段 ".数字", 输入是心跳里的短字符串,
// 复杂度可控且没有编译期依赖。第三段允许带后缀(如 "2-rc1" → 2)。
func parseVersionTriple(version string) (major, minor, patch int, ok bool) {
	s := strings.TrimSpace(version)
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, 0, 0, false
	}

	parts := strings.SplitN(s[start:], ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, part := range parts {
		end := 0
		for end < len(part) && part[end] >= '0' && part[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, 0, 0, false
		}
		n, err := strconv.Atoi(part[:end])
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// fileToken 把 Drive 路径编码为可安全放进 URL 路径的 file_id
//
// base64url 无填充: 只含 [A-Za-z0-9_-], 不需要再做 URL 转义;
// 对 agent 而言它是不可解析的 opaque 值(节点不解码, 只回传给 master).
func fileToken(gdPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(gdPath))
}

// decodeFileToken 把 file_id 还原为 Drive 路径
//
// master 侧下载直线接口要按 file_id 找文件, 因此这里必须解码;
// 解码失败(节点传了别的 client 编出来的串)是客户端错误, 调用方按 400 处理.
func decodeFileToken(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", errors.New("file_id 不是合法的 base64url 编码")
	}
	gdPath := string(raw)
	if strings.TrimSpace(gdPath) == "" {
		return "", errors.New("file_id 解码后为空")
	}
	return gdPath, nil
}

// signClientURL 签发节点上的客户端地址: {base}/dl/{file_id}?e={expiry}&s={sign}
//
// 签名 = HMAC-SHA256(key = sign_key 的 32 字节, msg = signMessage(fileID, expiry)),
// 小写 hex 输出. 同一算法只有这一处实现, 任一处改动都要同步 agent 侧的验签测试向量.
//
// 返回的完整地址含签名参数 s, 不得写进日志(只记节点与文件).
func signClientURL(rec *agentRecord, gdPath string, expiresAt time.Time) (string, error) {
	base := agentBaseURL(rec)
	if base == "" {
		return "", errors.New("节点地址不可推导")
	}

	key, err := hex.DecodeString(rec.SignKey)
	if err != nil || len(key) == 0 {
		return "", fmt.Errorf("节点 %s 的 sign_key 不是合法的十六进制串, 无法签发客户端地址", rec.ID)
	}

	fileID := fileToken(gdPath)
	expiry := strconv.FormatInt(expiresAt.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write(signMessage(fileID, expiry))
	sign := hex.EncodeToString(mac.Sum(nil))

	return base + "/dl/" + fileID + "?e=" + expiry + "&s=" + sign, nil
}

// signClientURLV2 签发 hub 直连的客户端地址(v0.4.2, N1)
//
//	{base}/dl/{file_id}?e={expiry}&u={hubBase}&f={driveFileID}&s={sign}
//
// 签名 = HMAC-SHA256(key = sign_key 的 32 字节, msg = signMessageV2(...)) 的小写 hex。
// u/f 先取原值算签名, 再按查询参数编码进 URL(agent 侧解码后按原值复算)。
//
// 返回的完整地址含签名参数 s 与 hub 内网地址 u, 不得写进日志(只记节点与文件)。
func signClientURLV2(rec *agentRecord, gdPath, hubBase, driveFileID string, expiresAt time.Time) (string, error) {
	base := agentBaseURL(rec)
	if base == "" {
		return "", errors.New("节点地址不可推导")
	}
	if strings.TrimSpace(hubBase) == "" || strings.TrimSpace(driveFileID) == "" {
		return "", errors.New("hub 直连基址或 Drive 文件 id 为空")
	}

	key, err := hex.DecodeString(rec.SignKey)
	if err != nil || len(key) == 0 {
		return "", fmt.Errorf("节点 %s 的 sign_key 不是合法的十六进制串, 无法签发客户端地址", rec.ID)
	}

	fileID := fileToken(gdPath)
	expiry := strconv.FormatInt(expiresAt.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write(signMessageV2(fileID, expiry, hubBase, driveFileID))
	sign := hex.EncodeToString(mac.Sum(nil))

	return base + "/dl/" + fileID +
		"?e=" + expiry +
		"&u=" + url.QueryEscape(hubBase) +
		"&f=" + url.QueryEscape(driveFileID) +
		"&s=" + sign, nil
}

// PickAndSign 为一次播放选点并签发客户端地址
//
// 返回的地址可直接 302 给客户端, 媒体字节不经过本网关。三类结果由错误区分,
// 调用方必须区别对待:
//
//   - ErrDisabled: 功能未启用(调用方本应已判断, 这里只是兜底);
//   - ErrNoAgent: 当前没有可调度节点 —— 这是唯一适用 fallback-to-local 分支的结果;
//   - 其它错误: 内部故障(注册表读取/签发失败), 调用方应记 WARN 后走原有流程.
//
// 本函数只在此处打一条调度日志, 且绝不包含签名参数与完整地址.
func PickAndSign(gdPath string) (string, error) {
	cfg := agentNetworkConfig()
	if !cfg.IsEnabled() {
		return "", ErrDisabled
	}
	if strings.TrimSpace(gdPath) == "" {
		return "", errors.New("Drive 路径为空, 无法调度节点")
	}

	now := time.Now()
	rec, err := defaultRegistry.schedule(now, time.Duration(cfg.OfflineSeconds)*time.Second, cfg.ScheduleStrategy())
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", ErrNoAgent
	}

	ttl := now.Add(cfg.ClientURLTTL())

	// v2 hub 直连(v0.4.2, N2): 门槛满足时签 v2(u=hub 内网基址, f=Drive 文件 id),
	// 客户端 307 之后节点直连 hub、稳态零回访; 任一条件不满足维持 v1(现行为,
	// 旧版本节点拿到的东西逐字节不变)。
	if hubBase, driveFileID, ok := hubDirectV2Target(gdPath, rec, now); ok {
		url, verr := signClientURLV2(rec, gdPath, hubBase, driveFileID, ttl)
		if verr != nil {
			// 只可能是节点记录自身的问题(sign_key 非法 / 地址不可推导),
			// v1 签发同样会失败 —— 不吞错, 让调用方按内部故障处理。
			return "", verr
		}
		logPick(rec, cfg, gdPath, ", 上游: hub 直连")
		return url, nil
	}

	url, err := signClientURL(rec, gdPath, ttl)
	if err != nil {
		return "", err
	}
	logPick(rec, cfg, gdPath, "")
	return url, nil
}

// logPick 记一条调度日志(格式与 v0.4.1 一致, suffix 为空时逐字不变)
//
// suffix 只在 v2 hub 直连时非空(如 ", 上游: hub 直连"), 供运维一眼确认本次
// 307 走的是哪条链路。绝不打印签名参数与完整地址。
func logPick(rec *agentRecord, cfg *config.AgentNetwork, gdPath, suffix string) {
	// priority 策略下追加优先级数字: 活跃流照常打印, 便于对比两种策略的选点差异
	if cfg.ScheduleStrategy() == config.ScheduleStrategyPriority {
		logf(colors.Green, "调度到节点: %s(%s), 活跃流: %d, 优先级: %d, 文件: %s%s",
			rec.Name, rec.ID, rec.ActiveStreams, rec.Priority, gdPath, suffix)
		return
	}
	logf(colors.Green, "调度到节点: %s(%s), 活跃流: %d, 文件: %s%s",
		rec.Name, rec.ID, rec.ActiveStreams, gdPath, suffix)
}
