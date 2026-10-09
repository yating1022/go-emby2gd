// Package agentnet 实现 agent 代理网络的 master 侧
//
// 本网关作为 master: 维护节点注册表(注册 / 心跳 / 启停 / 删除),
// 在有可用节点时把客户端 302 到节点上的签名地址, 媒体字节不经过本网关。
//
// 协议细节以任务 10-08-agent-proxy-network 的 design 为准, 其中三条硬约束:
//
//   - 客户端 URL 的签名消息逐字为 "v1\n<file_id>\n<e>", file_id 是
//     base64url(无填充) 编码的 Drive 路径; 签名算法改动即协议破坏;
//   - 节点凭据(agent_secret / sign_key)与 Google 凭据只能出现在
//     下发给对应节点的响应里, 不得进日志、不得落盘到日志文件;
//   - 心跳只更新内存(每 15 秒一次不允许触发磁盘写), 其余变更先落盘再回响应。
package agentnet

import (
	"fmt"

	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs"
	"github.com/AmbitiousJun/go-emby2openlist/v2/internal/util/logs/colors"
)

// logf 输出带 [agent 网络] 前缀的模块日志
//
// 任何调用点都不得把 secret / sign_key / Google 凭据 / 签名参数 s /
// 完整签名 URL 传进来(见包注释的硬约束)。
func logf(c colors.C, format string, v ...any) {
	s := fmt.Sprintf(format, v...)
	logs.Raw("%s%s\n", logs.Now(), colors.WrapColor(c, "[agent 网络] "+s))
}
