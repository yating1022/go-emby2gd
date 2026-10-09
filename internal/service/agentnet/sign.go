package agentnet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
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

	url, err := signClientURL(rec, gdPath, now.Add(cfg.ClientURLTTL()))
	if err != nil {
		return "", err
	}

	// priority 策略下追加优先级数字: 活跃流照常打印, 便于对比两种策略的选点差异
	if cfg.ScheduleStrategy() == config.ScheduleStrategyPriority {
		logf(colors.Green, "调度到节点: %s(%s), 活跃流: %d, 优先级: %d, 文件: %s",
			rec.Name, rec.ID, rec.ActiveStreams, rec.Priority, gdPath)
	} else {
		logf(colors.Green, "调度到节点: %s(%s), 活跃流: %d, 文件: %s", rec.Name, rec.ID, rec.ActiveStreams, gdPath)
	}
	return url, nil
}
