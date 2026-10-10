# implement：agent IPv6 客户端接入（顺序清单）

> 主模块为主；**agent 侧零代码改动**。`export PATH=$PATH:/usr/local/go/bin`；不 commit。
> 状态（2026-10-10）：S1–S3 + 独立检查完成。master 侧纯补测（v6 用例含独立 HMAC 复算）；
> 安装脚本幂等路径 `--public-url`（锚定 sed + 原子替换 + 失败不动文件）；沙盒用例 5 组。
> **检查自修 1 类缝隙**：预检与 master 校验的 4 处差异（userinfo/`?#`/方括号未闭合/裸 v6）
> 均已主动拒绝（裸 v6 更严并给出正确写法）；spec §3.8/§3.10/§6 同步。
> 出站（S1/S2 原计划）已按用户范围修订**全部撤销**（上游四文件与基线逐字节 SAME）。
> 待办：S4 真机 v6 验证（随 v0.3.1 发布执行）。

- [x] S1 master 侧补测（生产代码零改动）：`parsePublicBaseURL` v6、`agentBaseURL` v6 `LastIP`、`sign.go` v6 基址签名全链（独立 HMAC 复算）
- [x] S2 安装脚本：幂等路径 `--public-url` 锚定替换 + 预检（对齐 master 规则）+ 帮助文本 v6 示例；沙盒用例
- [x] S3 测试对照：全量对照基线（既有 5 失败包原样）+ `-race` 相关包 + gofmt/vet/`bash -n`
- [x] S4（随发布）真机：Zouter 配置 v6 → master Address 为 v6 → v6 直连 12MB/s；spec 已补 v6 小节（§3.8/§3.10/§6）

## 回滚点

- 生产 Go 代码零改动；脚本替换单行可逆（改回旧值即回默认推导）；随 v0.3.1 发布。
