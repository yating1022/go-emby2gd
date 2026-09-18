<div align="center">
  <img height="150px" src="./assets/logo.png"></img>
</div>

<h1 align="center">go-emby2gd</h1>

<div align="center">
  <img src="https://img.shields.io/badge/license-GPL--3.0-blue"></img>
  <img src="https://img.shields.io/badge/go-1.26-00ADD8"></img>
</div>

<div align="center">
  Emby 反向代理 + 网盘直链串流。strm 播放由本机代理拉流, 支持从 GD 管理面板换取 Google Drive 直链。
</div>

---

## 这是什么

一个 Go 编写的 **Emby 反向代理**。核心是一件事:

**Emby 说「这个文件在某个路径」, 本项目把这个路径换成一条可直接拉取的字节流, 自己代理给播放器。**

现在支持两类来源:

| 来源 | 触发条件 | 做法 |
|---|---|---|
| **GD 管理面板直链** | strm 内容命中挂载前缀 | 去掉前缀得到团队盘内路径 → 调面板 `/api/dl` 换直链与请求头 → 本机带凭据拉流 |
| **strm 直链代理** | strm 地址命中 `emby.strm.proxy.domains` | 本机请求上游(网关), 跟随重定向解析出直链 → 本机代理拉流 |

两者都不再让客户端 302 到外部地址, **字节流由本机转发**。任何环节失败都会回退到回源(Emby 直读), 播放不中断。

## 与上游的关系

本项目是 [AmbitiousJun/go-emby2openlist](https://github.com/AmbitiousJun/go-emby2openlist) 的二次开发分支,
按 GPL-3.0 分发。在此基础上主要做了两件事:

1. 新增 **GD 管理面板直链**取流路径(`internal/service/gdrive/`)
2. 新增 **strm 直链代理**(`internal/service/streamproxy/`), 把网盘直链的解析与拉流收到本机

上游的其余能力(OpenList 资源、转码 m3u8、本地目录树、字幕、图片、剧集排序等)都完整保留, 见下文「保留的能力」。

## 播放链路

```
        Emby                本项目                          上游
          │                   │                              │
   ① PlaybackInfo ──────────▶ │  反代 + 缓存                 │
          │                   │                              │
   ② /videos/<id>/stream      │                              │
          │                   │                              │
          │            取 strm 内容 / 媒体路径                 │
          │                   │                              │
          │         ┌── 命中挂载前缀 ──▶ 面板 /api/dl ────────▶ │ 换直链 + 请求头
          │         │                 │                      │
          │         └── 命中代理前缀 ──▶ 请求网关 ────────────▶ │ 跟重定向解析直链
          │                   │                              │
          │                   ▼                              ▼
   ③ 字节流 ◀────── 本机流式转发 ◀──────────── 带上游给的请求头拉流
```

## 功能

### 本项目的核心路径

- **GD 管理面板直链**: strm 内容命中 `gdrive.mount-prefix` 时, 去掉前缀得到团队盘内路径, 交给面板换取直链与请求头, 由本机带凭据拉流。面板返回的 `Authorization` 是**账号级**凭据, 只在本机与 Google 之间使用 —— 不回写客户端、不进日志、不落盘
- **strm 直链代理**: 命中 `emby.strm.proxy.domains` 的 strm 地址由本机代理。支持手动跟随重定向(带跳数上限)、直链缓存、失效重试
- **令牌与直链缓存**: 账号级令牌全局一份, 直链按路径缓存。缓存余量严格小于面板的提前刷新窗口, 播放跨过令牌有效期时透明换新, 不中断
- **回退**: 以上任一环节失败且尚未写出响应时, 回退到回源(Emby 直读), 播放可用性不受新功能影响

### 保留的能力

- Emby 接口反代与响应缓存(PlaybackInfo / 列表 / 图片 / 字幕)
- 本地媒体: 同源时 302 直连, 跨网时代理回源
- 自定义 CSS / JS 注入
- 内置 Web 平台(`/ge2o/web`): 首页、实时日志
- OpenList 资源获取、本地目录树、转码 m3u8、emby→网盘路径映射、视频预览、音乐信息
- websocket 代理、cors 调整、剧集排序

> 本项目的实际部署只用到了 GD 面板直链、strm 直链代理与响应缓存;
> 其余能力保留可用, 但需要在 `config.yml` 里配置对应段落才会生效。

## 部署

### 编译

```bash
export PATH=$PATH:/usr/local/go/bin

# 1 前端产物: web/embed.go 用 //go:embed all:dist, 而 web/dist 不入库,
#   干净的 checkout 必须先把前端构建出来, 否则 Go 编译不过。
#   仓库自带的脚本会做完整流程(在 web/src 里 npm ci && npm run build,
#   再把 src/build/client 挪到 web/dist):
./build_web.sh

# 2 交叉编译
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w" -o ge2o-linux-amd64 .
```

> 只想编 Go 的话, 也可以先 `mkdir -p web/dist` 造一个空目录让 `go:embed` 通过 —— 
> 但那样产出的二进制里没有前端页面, `/ge2o/web` 会打不开。

### 装到服务器

```bash
# 1 落到数据目录
mkdir -p /opt/ge2o && install -m 755 ge2o-linux-amd64 /opt/ge2o/ge2o

# 2 配置: 从 config-example.yml 抄一份改
cp config-example.yml /opt/ge2o/config.yml
#    !! 含 Token 等凭据, 权限必须收紧 !!
chmod 600 /opt/ge2o/config.yml
```

### systemd 服务

```ini
# /etc/systemd/system/ge2o.service
[Unit]
Description=ge2o - Emby Reverse Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/ge2o
# -dr 数据根目录(读 config.yml) / -p HTTP 端口 / -ps HTTPS 端口
ExecStart=/opt/ge2o/ge2o -dr /opt/ge2o -p 8099 -ps 8098
Restart=always
RestartSec=3
StandardOutput=append:/var/log/ge2o/ge2o.log
StandardError=append:/var/log/ge2o/ge2o.log

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now ge2o
```

### 启动参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-dr` | `.` | 程序数据根目录, 从它下面读 `config.yml` |
| `-p` | 8095 | HTTP 监听端口 |
| `-ps` | 8094 | HTTPS 监听端口(未启用 ssl 时不会监听) |
| `-version` | | 打印版本后退出 |

## 配置

完整说明见 [config-example.yml](./config-example.yml), 这里只列关键几段。

### `gdrive` —— GD 管理面板直链

```yaml
gdrive:
  enable: true
  # 面板地址; 结尾的 '/' 会被自动去掉
  api-base: "https://gd.example.com"
  # 面板的【直链服务】Token, 也可用环境变量 GDRIVE_API_TOKEN 覆盖(非空即覆盖)
  api-token: ""
  # strm 里的挂载点前缀, 去掉后剩下的就是团队盘内路径
  mount-prefix: /home/googleDrive
```

- 面板有**两枚** Token(直链服务 / 缓存服务), **不通用**, 用错会得到 401
- `enable: false`(默认) 时行为与未部署本功能完全一致
- 路径必须在面板侧有条目, 否则返回 `PATH_NOT_IN_CACHE`; 可让面板侧开启实时回退, 或先缓存该文件
- 面板报错时日志会原样打出它的中文 `message`, 随后回退到回源

### `emby.strm.proxy` —— strm 直链代理

```yaml
emby:
  strm:
    path-map:
      - /home/googleDrive =>        # 右侧留空表示直接删掉该前缀
    proxy:
      enable: true
      domains:                      # 命中任一前缀的 strm 由本机代理
        - https://vault.example.com
      max-concurrent-streams: 16    # 并发传输上限, 0 表示不限
      link-cache-expired: 10m
```

### 其他

| 段 | 说明 |
|---|---|
| `emby.host` | 上游 Emby 地址, 反代与回源的目标 |
| `emby.proxy-error-strategy` | 代理异常时的策略: `origin` 回源处理 / `reject` 直接返回错误。只有这两个合法值 |
| `emby.download-strategy` | 下载接口策略: `origin` 代理到源服务器 / `direct` 取直链后重定向 / `403` 拒绝 |
| `cache` | 响应缓存开关与过期时间; **媒体字节流不参与缓存** |
| `ge2o.api-secret` | 访问 `/ge2o/web` 等自有接口的密钥 |
| `ssl` | 是否启用 HTTPS |

## 排查

```bash
# 一次播放的完整链路
grep '\[直链代理\]' /var/log/ge2o/ge2o.log
```

正常应看到:

```
[直链代理] 检测到 Google Drive 挂载路径: /home/googleDrive/影视库/x.mkv -> /影视库/x.mkv
[直链代理] 已换取直链: /影视库/x.mkv, expires_at=...
[直链代理] 开始传输: https://www.googleapis.com/..., 客户端 Range: "bytes=0-"
[直链代理] 传输完成: 已发送 ... 字节
```

判读要点:

- **`已换取直链` 出现次数远少于 `开始传输`** 是正常的 —— 缓存命中时不会重新打面板
- `传输中断 ... context canceled` 多半是播放器缓冲够了主动断连, 不是故障
- 只有 `检测到挂载路径` 却没有 `开始传输`, 说明面板那一跳失败了, 继续往下看 `回源处理`
- 看到面板的中文报错文案 = 面板侧的问题(路径没缓存、Token 不对等), 不是本机故障

## 回滚

```bash
# 1 保留一份升级前的二进制与配置
cp -a /opt/ge2o/ge2o /opt/ge2o/ge2o.bak
cp -a /opt/ge2o/config.yml /opt/ge2o/config.yml.bak

# 2 出问题先关功能开关(不用改代码)
#    config.yml 里把 gdrive.enable / emby.strm.proxy.enable 置 false
#    然后 systemctl restart ge2o

# 3 要回到旧二进制
mv /opt/ge2o/ge2o.bak /opt/ge2o/ge2o && systemctl restart ge2o
```

## 许可与致谢

本项目基于 [AmbitiousJun/go-emby2openlist](https://github.com/AmbitiousJun/go-emby2openlist)
二次开发, 按 **GNU GPL-3.0** 分发 — 见 [LICENSE](./LICENSE)。

上游项目及其作者的原始工作构成本项目的基础, 版权归原作者所有;
本分支的修改同样以 GPL-3.0 授权。
