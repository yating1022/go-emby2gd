# design：agent 侧（搬迁落地说明）

> 本任务不引入新协议、新架构。技术依据 = 父任务 `design.md`（§2.3 余量链、§2.8 安装与发布契约）。
> 冲突时以父 design 为准；改协议 = 先改父 design。

## 落点

| 落点 | 说明 |
|---|---|
| `./agent/**` | 嵌套独立 module（路径 `github.com/yating1022/go-emby2gd/agent` 不变），零第三方依赖保持。根 module 构建（build.sh / `go ./...`）自动跳过嵌套 module，无交叉影响 |
| `internal/service/agentnet/installshell/agent-install.sh` | 安装脚本**唯一正本**（子任务 A 用 go:embed 供给 `/install.sh`；发布快照含整树，仓库侧同样可寻） |
| `.github/workflows/release-agent.yml` | 与主仓库 build/docker 工作流并存，互不干扰 |

## 唯一的行为修正 = 余量

参考稿 5min 余量是在「master 直接持 Google OAuth」的语境下取的；本项目 master 背后还有 GD 面板
（refresh-ahead 60s），余量链必须满足 **agent 25s < master 30s < 面板 60s**（父 design §2.3）：

- 5min ≥ 30s 时：agent 在 `expires_at−5min` 就要刷新 → master 缓存（`−30s` 才失效）原样返回
  **同一份** token → agent 重算余量仍不足 → 该窗口内**每次 Range 请求都重拉**（可见病灶同
  `gdrive-panel.md` §3.1 描述的 5 分钟余量）；
- 25s 时：agent 在 `−25s` 刷新 → master 缓存已失效 → 真调面板（面板已在 60s 发放窗口内）→ 拿到新令牌。

改动范围：常量 + 注释（引用本链条与依据）+ 断言测试；**缓存结构、单飞、Enabled 语义全部不动**。

## 不做

- 不改协议面任何字段/路径/格式/资产名（master 侧与发布契约已冻结）。
- 不引入第三方依赖；不做自动升级；不动参考仓库（只读）。
