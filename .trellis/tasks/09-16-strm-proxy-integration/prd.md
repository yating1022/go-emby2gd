# strm 代理接入播放链路与端到端验证

> 父任务: `09-16-strm-proxy-play`。契约见父任务 `design.md`。
> 前置: `09-16-cache-stream-passthrough`、`09-16-strm-proxy-core` 均已完成。

## Goal

把 `streamproxy` 接进 `Redirect2OpenlistLink` 的 strm 分支, 打通"命中前缀 → 关闭响应缓存 → 代理播放"这条链路, 落实失败回退语义, 并完成真实环境下的端到端验证。

这是**唯一会改变线上播放行为**的子任务, 因此验收以真实播放为准, 不以单元测试通过为准。

## Requirements

- **R1 决策接入**: 在 `internal/service/emby/redirect.go` 的 strm 分支（现 `redirect.go:95-106`）中, 于 `MapPath` 之后、`getFinalRedirectLink` 之前插入代理判断。
- **R2 缓存旁路无需 handler 参与**: 字节流路由已由 `09-16-cache-stream-passthrough` 移出响应缓存白名单, 到达 handler 时 `c.Writer` 就是原始 writer。**本子任务不得**在 handler 中新增任何缓存旁路调用（那会是无用代码）。需要核实的是该前置改动确实生效 —— 见 AC0。
- **R3 回退语义**: 按父 `design.md` §8 实现。代理未写入响应时的失败一律回退到**现有 302 流程**, 不再叠加 `checkErr`。
- **R4 未命中不改变行为**: 未命中前缀时, 代码路径与日志输出与改动前一致（仅多一条 `未命中任何代理前缀`）。
- **R5 日志贯通**: 补齐父 `design.md` §7 中属于本层的 L1、L2、L2'。
- **R6 端到端验证**: 完成父任务 `implement.md` 的 V1-V6 验证, 并留下可复查的证据（日志片段、内存数据）。
- **R7 覆盖 `original` 路由**: `ProxyOriginalResource` 最终也汇入 `Redirect2OpenlistLink`, 需确认该路径同样生效且不产生重复决策。

## 目标代码形态（`redirect.go` strm 分支）

```go
	// 4 如果是远程地址 (strm), 重定向处理
	if urls.IsHttpRemote(embyPath) {
		finalPath := config.C.Emby.Strm.MapPath(embyPath)

		// 4.1 命中代理前缀时, 由本项目请求上游并流式代理给客户端, 不再 302
		//
		// 注意: 这里不需要处理响应缓存 —— 字节流路由已不在缓存白名单内,
		// c.Writer 就是原始 writer, 见子任务 09-16-cache-stream-passthrough
		if config.C.Emby.StrmProxyEnabled() {
			// L1 放在开关启用块内: 开关关闭时不得产生任何 [直链代理] 日志噪音 (R4/R10)
			logs.Info("[直链代理] 检测到 strm 远程地址: %s", embyPath)

			if prefix, ok := streamproxy.MatchDomain(finalPath); ok {
				logs.Info("[直链代理] 命中代理前缀: %s", prefix)

				// 必须在 Proxy 之前触发: Proxy 会阻塞到本次传输彻底结束,
				// 放在它之后触发要等整部片子播完才会发出, 等于没有触发
				go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)

				written, proxyErr := streamproxy.Proxy(c.Writer, c.Request, finalPath)
				if written {
					return
				}
				logs.Error("[直链代理] 代理失败, 回退原有 302 流程: %v", proxyErr)
			} else {
				logs.Info("[直链代理] 未命中任何代理前缀, 走原有 302 流程")
			}
		}

		// 4.2 原有 302 流程 (含 internal-redirect-enable)
		finalPath = getFinalRedirectLink(finalPath, c.Request.Header.Clone())
		logs.Success("重定向 strm: %s", finalPath)
		c.Header(cache.HeaderKeyExpired, cache.Duration(time.Minute*10))
		c.Redirect(http.StatusTemporaryRedirect, finalPath)

		// 异步发送一个播放 Playback 请求, 触发 emby 解析 strm 视频格式
		go sendOpenStreamPlaybackInfoReqToOrigin(itemInfo)

		return
	}
```

**注意**: `written == false` 且 `proxyErr == nil` 属于不该出现的状态（`streamproxy.Proxy` 的契约是"未写入则必带回错误"）。实现时按"回退并记 Error"处理即可, 但**不要**写成静默忽略 —— 这类不一致恰恰是排查时最需要看到的。

### 两处与初版设计不同的地方（实现阶段修订, 已同步到上面的片段）

1. **L1 从开关外移到了开关内**。初版片段把 `检测到 strm 远程地址` 放在 `if StrmProxyEnabled()` 之前, 与 R4/R10（开关关闭时行为与日志都与改动前一致）冲突。移入开关内后: 开关关 → 零 `[直链代理]` 日志; 开关开 + 命中 → L1→L2; 开关开 + 未命中 → L1→L2'。
2. **`sendOpenStreamPlaybackInfoReqToOrigin` 必须在 `Proxy` 之前触发**。`streamproxy.Proxy` 会**阻塞到整次传输结束**, 若按初版片段放在 `if written { return }` 之前, 这次探测要等整部片子播完才发出, 完全失去意义。改到 `Proxy` 之前异步触发。
   代价是: 代理失败回退到 302 时, 该探测会被触发两次（302 路径本来就有一处）。重复触发是幂等的探测请求, 无害; 换来的是两条路径都保证在传输开始前触发。

   **该调用不可省的依据**: 它给 Emby 发 `IsPlayback=true&AutoOpenLiveStream=true` 的 PlaybackInfo 请求, 作用是让 Emby 真正打开并探测远程流, 拿到时长与编码格式。`playbackinfo.go:141` 在 `IsRemote` 时也会触发同一函数, 说明这是远程媒体元数据链路的既有依赖。代理模式下客户端播放的是同一份 strm 内容, 该依赖依然成立, 因此不应因为"我们在代理"就省略。

## Acceptance Criteria

- [ ] **AC0** 前置改动已生效: 请求 `/Videos/{id}/stream` 时 `c.Writer` 不是缓冲 writer（可在 `RequestCacher` 的跳过分支加临时日志确认一次, 验完删除）。
- [ ] **AC1** 命中前缀的视频播放成功, 且播放流量不经过 Emby（V1）。
- [ ] **AC2** 拖动进度条 seek 正常（V2）。
- [ ] **AC3** 播放 600MB+ 文件时进程内存不出现百 MB 级跃升（V3）。
- [ ] **AC4** 一次播放的日志可用 `grep '\[直链代理\]'` 还原完整链路（V4）。
- [ ] **AC5** 未命中前缀的 strm、本地媒体、字幕、图片接口行为与改动前一致（V5）。
- [ ] **AC6** `emby.strm.proxy.enable: false` 时行为与改动前完全一致（V5）。
- [ ] **AC7** `original` 路由（`ProxyOriginalResource` 汇入路径）行为正确, 无重复代理或重复响应写入。
- [ ] **AC8** 代理失败（可构造: 临时把 `domains` 指向一个不可达地址）时, 客户端仍收到原有 302, 日志有明确 Error。
- [ ] **AC9** `go build ./...`、`go vet ./internal/...` 通过。

## Constraints

- 不改动 `internal/service/emby` 中与本需求无关的分支。
- 不新增路由规则（本需求复用既有 `Reg_ResourceStream` / `Reg_ItemDownload` / `Reg_ResourceOriginal` 规则）; 若确有必要新增, 必须同时更新 `internal/constant/constant.go` 与 `internal/web/route.go` 的规则表。
- 不在 handler 中新增缓存相关调用; 缓存旁路由路由白名单层面解决。

## Out of Scope

- 移除 OpenList 相关代码 —— `09-16-remove-openlist`。
- 转码 / m3u8 播放链路的代理。
- HEAD 请求透传（父 PRD Q2）。

## Notes

- 本子任务完成后**不要立即开始** `09-16-remove-openlist`, 先确认端到端播放稳定, 否则问题会与删除产生的连带影响混在一起。
- 若 V1-V6 有任何一项不通过, 按父 `implement.md` 的回滚点处理: 优先用 `emby.strm.proxy.enable: false` 立即恢复可用性, 再定位问题。
