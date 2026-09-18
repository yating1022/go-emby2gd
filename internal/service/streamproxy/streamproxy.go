package streamproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/service/gdrive"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/bytess"
)

var (
	// slotsOnce 保证并发信号量只初始化一次
	slotsOnce sync.Once
	// streamSlots 并发传输信号量, 容量即最大并发数
	//
	// 上限为 0 时保持 nil, 表示不参与控制
	streamSlots chan struct{}
	// slotLimit 信号量对应的并发上限
	slotLimit int
	// activeStreams 当前活跃传输数, 仅用于日志观测, 不参与控制逻辑
	activeStreams atomic.Int64
)

// initSlots 按当前配置初始化并发信号量
//
// 只在首次使用时执行一次: config.C 在启动后只读, 运行期不会变化。
// 不做"上限变化时重建 channel": 重建会让正在等待槽位的请求永久挂在被丢弃的
// 旧 channel 上, 同时释放方会从新 channel 收走别人的令牌, 造成计数丢失。
func initSlots() {
	slotsOnce.Do(func() {
		if cfg := proxyConfig(); cfg != nil {
			slotLimit = cfg.MaxConcurrentStreams
		}
		if slotLimit > 0 {
			streamSlots = make(chan struct{}, slotLimit)
		}
	})
}

// acquireSlot 获取一个传输槽位
//
// 上限为 0 时立即返回;
// 满载时等待而不是拒绝, 等待期间响应客户端上下文取消, 避免连接堆积。
// 拿不到槽位时返回错误, 此时调用方尚未写出任何响应, 可以正常回退。
func acquireSlot(ctx context.Context) error {
	initSlots()

	if streamSlots == nil {
		// 未设置上限, 只输出活跃数
		logInfof("当前活跃传输: %d", activeStreams.Add(1))
		return nil
	}

	select {
	case streamSlots <- struct{}{}:
	default:
		logWarnf("并发传输已达上限 %d, 等待槽位, 当前活跃: %d", slotLimit, activeStreams.Load())
		select {
		case streamSlots <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("等待传输槽位时客户端已断开: %w", ctx.Err())
		}
	}

	logInfof("当前活跃传输: %d/%d", activeStreams.Add(1), slotLimit)
	return nil
}

// releaseSlot 释放传输槽位
//
// 必须在 Proxy 中用 defer 调用, 覆盖正常结束、上游出错与客户端中断三条路径。
// 使用非阻塞接收, 不会在这里阻塞。
func releaseSlot() {
	initSlots()
	activeStreams.Add(-1)

	if streamSlots == nil {
		return
	}
	select {
	case <-streamSlots:
	default:
	}
}

// Proxy 将 rawURL 指向的媒体字节流代理给客户端
//
// written 为 true 表示响应已经开始写入, 调用方不得再做任何回退;
// written 为 false 且 err 非空表示尚未写入任何响应, 调用方可按既有策略回退。
//
// 调用方应先通过 MatchDomain 判断是否命中代理前缀;
// 本函数内部会再校验一次, 未开启代理或未命中前缀时直接返回错误, 不发起任何上游请求。
func Proxy(w http.ResponseWriter, r *http.Request, rawURL string) (written bool, err error) {
	if w == nil || r == nil {
		return false, errors.New("参数为空")
	}
	if !ProxyEnabled() {
		return false, errors.New("strm 直链代理未开启")
	}
	if _, ok := MatchDomain(rawURL); !ok {
		return false, fmt.Errorf("地址未命中任何代理前缀: %s", rawURL)
	}

	// 1 归一化地址
	normalized, err := NormalizeURL(rawURL)
	if err != nil {
		logErrorf("代理失败: %v", err)
		return false, err
	}
	if normalized != rawURL {
		logInfof("归一化地址: %s (原始: %s)", normalized, rawURL)
	}

	// 2 获取并发槽位: 必须在写出任何响应之前, 失败时才能正常回退
	if err := acquireSlot(r.Context()); err != nil {
		logErrorf("代理失败: %v", err)
		return false, err
	}
	defer releaseSlot()

	// 3 请求上游并解析出可流式读取的响应
	resp, err := resolveLink(r.Context(), normalized, r.Header)
	if err != nil {
		logErrorf("代理失败: %v", err)
		return false, err
	}
	defer resp.Body.Close()

	// 4 回写响应头并流式转发响应体
	return relay(w, r, resp)
}

// relay 回写响应头并把上游响应体流式转发给客户端
//
// Proxy 与 ProxyGDrive 共用本函数: 数据源不同, 但"回写响应头 -> 立即 flush ->
// 流式传输 -> 收尾日志"这套语义必须完全一致, 一处改动两处同时生效。
//
// written 为 true 表示响应已经开始写入, 调用方不得再做任何回退。
func relay(w http.ResponseWriter, r *http.Request, resp *http.Response) (bool, error) {
	// 1 回写响应头并立即 flush, 让首字节尽快到达客户端
	writeResponseHeader(w, resp)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// 2 流式回写响应体, 全程不整体缓冲
	logInfof("开始传输: %s, 客户端 Range: %q", responseURL(resp), r.Header.Get("Range"))
	start := time.Now()

	buf := bytess.CommonFixedBuffer()
	defer buf.PutBack()

	sent, copyErr := io.CopyBuffer(w, resp.Body, buf.Bytes())
	if copyErr != nil {
		if errors.Is(copyErr, context.Canceled) || r.Context().Err() != nil {
			logWarnf("传输中断: 已发送 %d 字节, err: %v", sent, copyErr)
		} else {
			logErrorf("传输失败: 已发送 %d 字节, err: %v", sent, copyErr)
		}
		return true, copyErr
	}

	logSuccessf("传输完成: 已发送 %d 字节, 耗时 %s", sent, time.Since(start))
	return true, nil
}

// fetchGDriveMedia GD 面板取流实现
//
// 抽成变量是为了让测试指向本地假端点; 生产环境始终是 gdrive.FetchStream。
var fetchGDriveMedia = gdrive.FetchStream

// ProxyGDrive 将 GD 管理面板提供的直链字节流代理给客户端
//
// gdPath 是团队盘内的文件路径(已去掉挂载前缀), 由调用方通过
// gdrive.MatchMountPath 解析得到; 面板负责把路径换成直链与请求头。
//
// written 的契约与 Proxy 完全一致: true 表示响应已经开始写入, 调用方不得再做
// 任何回退; false 且 err 非空表示尚未写入任何响应, 调用方可回源。
func ProxyGDrive(w http.ResponseWriter, r *http.Request, gdPath string) (written bool, err error) {
	if w == nil || r == nil {
		return false, errors.New("参数为空")
	}
	if !gdrive.IsEnabled() {
		return false, errors.New("Google Drive 直接取流未开启")
	}
	if strings.TrimSpace(gdPath) == "" {
		return false, errors.New("Google Drive 路径为空")
	}

	// 1 获取并发槽位: 必须在写出任何响应之前, 失败时才能正常回退
	if err := acquireSlot(r.Context()); err != nil {
		logErrorf("代理失败: %v", err)
		return false, err
	}
	defer releaseSlot()

	// 2 解析路径并请求媒体数据
	resp, err := fetchGDriveMedia(r.Context(), gdPath, r.Header.Get("Range"))
	if err != nil {
		logErrorf("代理失败: %v", err)
		return false, err
	}
	defer resp.Body.Close()

	// 3 回写响应头并流式转发响应体, 与 Proxy 共用 relay
	return relay(w, r, resp)
}

// writeResponseHeader 回写上游响应头
//
// 只回写白名单内的头, 逐跳头不写回客户端;
// 上游返回 206 但未携带 Accept-Ranges 时补写, 帮助播放器识别可拖动
func writeResponseHeader(w http.ResponseWriter, resp *http.Response) {
	header := w.Header()
	for _, key := range passthroughResponseHeaders {
		values := resp.Header.Values(key)
		if len(values) == 0 {
			continue
		}
		header.Del(key)
		for _, value := range values {
			header.Add(key, value)
		}
	}

	if resp.StatusCode == http.StatusPartialContent && header.Get("Accept-Ranges") == "" {
		header.Set("Accept-Ranges", "bytes")
	}

	// Location 可能是相对地址, 必须基于上游地址解析成绝对地址后再回写
	//
	// 上游对非跟随集合内的 3xx (300/303 等) 可能返回相对 Location。
	// 原样透传的话, 客户端会按本项目的 origin 去解析, 请求打到本项目自身的路由上
	// (通常 404), 而不是上游的同源路径 —— 语义是错的。
	if location := header.Get("Location"); location != "" {
		if base := responseURL(resp); base != "" {
			resolved, err := resolveRedirectLocation(base, location)
			if err != nil {
				logWarnf("解析上游 Location 失败, 按原值回写: %s, err: %v", location, err)
			} else {
				header.Set("Location", resolved)
			}
		}
	}

	w.WriteHeader(resp.StatusCode)
}

// responseURL 获取响应最终对应的请求地址
func responseURL(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return resp.Request.URL.String()
}
