// gd-agent：GD 代理网络的 agent 节点（数据面出口）。
//
// 零第三方依赖：HTTP / JSON / HMAC / 日志全部走标准库，
// 便于审计、供应链最小化，并支持 CGO_ENABLED=0 静态单二进制交叉编译。
module github.com/yating1022/go-emby2gd/agent

go 1.22
