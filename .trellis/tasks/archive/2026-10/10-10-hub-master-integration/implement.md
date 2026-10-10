# implement：master 侧接入（hub）顺序清单

> 主模块（本仓库根）；`export PATH=$PATH:/usr/local/go/bin`；不 commit。
> 前置：v0.3.2 发布冻结。

- [ ] S1 role 字段：enroll 可选 role（缺省 node、校验）、agents.json 兼容读取、admin 列表输出 + 单测
- [ ] S2 调度隔离：客户端候选集过滤 role==node（定位 10-09 调度实现点）+ 测试（hub 不入候选）
- [ ] S3 `hubFor(fileID)`：健康过滤 + 排序 + hash 取模；0/1/2 台与确定性单测
- [ ] S4 预热改向：hub 健康→POST /warm（直链/auth/regions、续播点尽力而为、3s 超时、异步）；失败/不健康→回退戳边缘；去重保持；httptest 假 hub 单测
- [ ] S5 播放上游改写：warm 接受→hub URL 链路（无 auth）；否则现状；回退链测试；日志无签名泄露
- [ ] S6 测试/回归：`hub-enable=false` 全量对照基线（5 个既有失败包对照式口径）；新增用例 `-race`、`-count=3` 无 flake；gofmt/vet；实现报告记录偏差

## 回滚点

- 配置默认 `hub-enable=false` + 新增文件为主；回滚=关开关（行为与现状逐字一致）；master 升级=compose rebuild + force-recreate（既有流程）。
