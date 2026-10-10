#!/usr/bin/env bash
# gd-agent 一键安装脚本（父任务 10-08-agent-proxy-network design §2.8）。
#
# 由 master（本网关）的 `/install.sh` 端点原样返回（路由注册在 catch-all 之前），
# 也在仓库里可直接执行：
#
#   curl -fsSL <master>/install.sh | sudo bash -s -- --master <master地址> --token <注册Token>
#
# 做四件事：
#   1. 按架构（amd64 / arm64）下载 agent 二进制，并用 checksums.txt 校验 sha256；
#   2. 装到 /usr/local/bin/gd-agent，建专用系统用户 gd-agent；
#   3. 一次性注册（`gd-agent enroll`）——凭据写进 /etc/gd-agent/config.env（0600）；
#   4. 写 systemd 单元（Restart=always）并 enable --now。
#
# ## 幂等（重跑 = 升级）
#
# 配置已存在且**没有** `--force-enroll` 时：只换二进制 + 重启服务，**不**重新注册。
# 这一点是刻意的：重新注册会轮换 secret/sign_key（父 design §6-3），
# 而正在跑的那个进程手里还是旧密钥，重装一次就会把一台好节点短暂踢下线。
# 只有要换注册 Token 时才用 `--force-enroll`；改 `--public-url` 不必——幂等路径
# 会锚定替换 config.env 里的 PUBLIC_BASE_URL 行（见 update_public_url）。
#
# ## ⚠️ enroll 之后必须把配置交给服务用户（否则服务起不来）
#
# `enroll` 由 root 执行（脚本整体跑在 sudo 下），因此它写出的
# `/etc/gd-agent/config.env` 属主是 `root:root` 0600；而 systemd 以专用用户
# `gd-agent` 运行 `serve` → 读配置 EACCES → 服务**反复重启且每次都失败**，
# 现象是「装完了但永远离线」。所以 enroll 成功后立刻
# `chown gd-agent:gd-agent` + `chmod 600`（agent 侧 `config.Load` 也为此专门写了
# 一条中文提示，见 `agent/internal/config/config.go`）。
#
# ## 两个为**自动化测试**而设的开关（生产不要用）
#
# - `--download-base <地址|目录|file://目录>`：覆盖下载源（本地联调 / 内网镜像）；
# - `GD_AGENT_INSTALL_ROOT=<目录>`：把整棵树装到该目录下（沙盒），并跳过所有
#   systemd 操作。冒烟测试靠它把「下载 → 校验 → 安装 → 幂等路径」整条路径
#   走完而不动这台机器（对应测试：internal/service/agentnet/installshell_sandbox_internal_test.go）。
set -euo pipefail

# agent 版本与资产名：**逐字**对应 .github/workflows/release-agent.yml 的三件资产
# （gd-agent-linux-amd64 / gd-agent-linux-arm64 / checksums.txt），改一处必须改两处。
ASSET_PREFIX="gd-agent-linux"
CHECKSUMS_ASSET="checksums.txt"

# GitHub Release 的直下地址（免 API 调用，`latest` 永远指向最新 tag）。
# 仓库 slug 与 `agent/go.mod` 的 module path 一致（跨端耦合点，改一处必须改两处）；
# 可用 GD_AGENT_GITHUB_REPO 覆盖（本地联调 / 内网镜像）。
# 守卫：覆盖值若仍是占位 `OWNER/REPO`，**不猜**，直接给中文错误。
GITHUB_REPO="${GD_AGENT_GITHUB_REPO:-yating1022/go-emby2gd}"
GITHUB_LATEST_BASE="https://github.com/${GITHUB_REPO}/releases/latest/download"

CONFIG_DIR="/etc/gd-agent"
CONFIG_FILE="${CONFIG_DIR}/config.env"
BIN_PATH="/usr/local/bin/gd-agent"
UNIT_PATH="/etc/systemd/system/gd-agent.service"
SERVICE_NAME="gd-agent"
SERVICE_USER="gd-agent"

MASTER=""
TOKEN=""
PUBLIC_URL=""
PORT="8790"
DOWNLOAD_BASE=""
FORCE_ENROLL=0

usage() {
  cat <<'USAGE'
gd-agent 安装脚本

用法：
  curl -fsSL <master>/install.sh | sudo bash -s -- --master <master地址> --token <注册Token> [选项]

选项：
  --master <地址>        master 的地址（如 http://1.2.3.4:4445 或 https://gd.example.com）
  --token <Token>        节点注册 Token（master 网页「节点」页生成，只显示一次）
  --public-url <地址>    本机对客户端可见的地址（NAT 后 / 端口映射时**必须**给，
                         否则 master 只能按源 IP 推导——那种地址客户端访问不到）。
                         重跑脚本时给出它 = 就地更新 config.env 的 PUBLIC_BASE_URL
                         行并重启服务（不重新注册、凭据不轮换）。
                         IPv6 节点示例：--public-url 'http://[2408:xxxx::1]:8790'
  --port <端口>          数据面监听端口（默认 8790，非特权端口）
  --download-base <地址> 覆盖二进制下载源：http(s) 地址、本地目录或 file:// 目录
                         （本地联调 / 内网镜像用；不给则从 GitHub Release 直下）
  --force-enroll         配置已存在时也重新注册（会轮换节点凭据）
  -h, --help             显示本帮助

重跑 = 升级：配置已存在且未给 --force-enroll 时，只替换二进制并重启服务。

客户端地址与 IPv6：
  · 默认按心跳来源 IP 推导（master 看到的地址）；只有客户端访问不到它时才需要
    --public-url。填 IPv6 地址时，只有具备 IPv6 的客户端能通过该地址取流。
  · 节点监听为双栈（v4/v6 客户端都能连），但节点防火墙需放行 v6 端口。
USAGE
}

log() { printf '%s %s\n' "[gd-agent]" "$*"; }
fail() {
  # 失败一律中文 + 退出码 1：这是用户唯一能看到的线索（日志里没有 token）。
  printf '%s %s\n' "[gd-agent] 安装失败：" "$*" >&2
  exit 1
}

# update_public_url 就地更新 config.env 的 PUBLIC_BASE_URL 行（重跑/升级路径）
#
# 只锚定替换 `^PUBLIC_BASE_URL=` 那一行，其余行原样保留；任何一步失败都
# **不改动原文件**并以中文报错——写坏它等于把一台在线节点变成"地址不可推导"
# （已建立的连接不受影响，但新播放会跳过该节点）。
update_public_url() {
  local file="$1"
  local value="$2"
  local tmp

  if ! grep -q '^PUBLIC_BASE_URL=' "${file}"; then
    fail "${file} 里没有 PUBLIC_BASE_URL 行，无法锚定替换；请手工补上该行（或加 --force-enroll 重新注册）"
  fi
  tmp="$(mktemp "${file}.tmp.XXXXXX")" ||
    fail "无法在 $(dirname "${file}") 创建临时文件（检查磁盘空间与权限）"

  # sed 替换串里这三个字符有特殊含义，必须转义；顺序不能反（先转义反斜杠自身）。
  local escaped="${value//\\/\\\\}"
  escaped="${escaped//&/\\&}"
  escaped="${escaped//|/\\|}"
  if ! sed "s|^PUBLIC_BASE_URL=.*|PUBLIC_BASE_URL=${escaped}|" "${file}" >"${tmp}"; then
    rm -f "${tmp}"
    fail "替换 ${file} 的 PUBLIC_BASE_URL 行失败（文件保持原样）"
  fi
  # 安全阀：替换后必须恰好一行 PUBLIC_BASE_URL。值里混进换行在参数校验处
  # 就会被拦下，这里再兜一次"锚错位置"之类的意外。
  local count
  count="$(grep -c '^PUBLIC_BASE_URL=' "${tmp}" || true)"
  if [[ "${count}" -ne 1 ]]; then
    rm -f "${tmp}"
    fail "替换 ${file} 后 PUBLIC_BASE_URL 行数不为 1（文件保持原样）"
  fi

  chmod 600 "${tmp}"
  if ! mv -f "${tmp}" "${file}"; then
    rm -f "${tmp}"
    fail "保存 ${file} 失败（文件保持原样）"
  fi
}

# --- 参数 -------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --master) MASTER="${2:-}"; shift 2 ;;
    --token) TOKEN="${2:-}"; shift 2 ;;
    --public-url) PUBLIC_URL="${2:-}"; shift 2 ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --download-base) DOWNLOAD_BASE="${2:-}"; shift 2 ;;
    --force-enroll) FORCE_ENROLL=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; fail "未知参数：$1" ;;
  esac
done

case "${PORT}" in
  '' | *[!0-9]*) fail "--port 必须是数字：${PORT}" ;;
esac
if ((PORT < 1 || PORT > 65535)); then
  fail "--port 超出范围（1-65535）：${PORT}"
fi

# --public-url 预检：幂等路径会把它直接写进 config.env（没有 master 帮忙校验），
# 坏值写进去 = 节点每个心跳都会被 master 400 拒绝，离线窗口（默认 45 秒）过后
# 就从调度池掉出去（现象和"装好了但一直离线"一样）。规则与 master 的
# parsePublicBaseURL 对齐：必须 http(s)、不允许空白（含换行——它是写进单行的值）、
# userinfo、查询参数、片段，以及写法不全的 IPv6 地址。
if [[ -n "${PUBLIC_URL}" ]]; then
  # 必须 http(s) 且带主机名（"http://" / "http:///" 这类空主机名直接拒绝）。
  case "${PUBLIC_URL}" in
    http://[!/]* | https://[!/]*) ;;
    *) fail "--public-url 必须是带主机名的 http(s) 地址（IPv6 示例：--public-url 'http://[2408:xxxx::1]:8790'）" ;;
  esac
  if [[ "${PUBLIC_URL}" != "${PUBLIC_URL//[[:space:]]/}" ]]; then
    fail "--public-url 不能包含空白字符（空格 / 制表符 / 换行）"
  fi
  # 去掉结尾多余的 '/'（enroll 也这么归一化）。
  while [[ "${PUBLIC_URL}" == */ ]]; do
    PUBLIC_URL="${PUBLIC_URL%/}"
  done

  # authority 段（"://" 之后、第一个 '/' '?' '#' 之前）用于形态检查；
  # 路径里出现这些字符不受影响（master 只对 host 做校验）。
  PUBLIC_URL_AUTHORITY="${PUBLIC_URL#*://}"
  PUBLIC_URL_AUTHORITY="${PUBLIC_URL_AUTHORITY%%[/?#]*}"
  if [[ "${PUBLIC_URL_AUTHORITY}" == *"@"* ]]; then
    fail "--public-url 不能包含用户名或密码（userinfo），只写 http://主机[:端口]"
  fi
  if [[ "${PUBLIC_URL}" == *"?"* || "${PUBLIC_URL}" == *"#"* ]]; then
    fail "--public-url 不能包含查询参数（?）或片段（#）"
  fi
  if [[ "${PUBLIC_URL_AUTHORITY}" == \[* ]]; then
    # IPv6：方括号要闭合、括号后只能跟端口、括号里不能为空。
    if [[ "${PUBLIC_URL_AUTHORITY}" != *\]* ]]; then
      fail "--public-url 的 IPv6 方括号未闭合（正确示例：--public-url 'http://[2408:xxxx::1]:8790'）"
    fi
    PUBLIC_URL_AFTER_BRACKET="${PUBLIC_URL_AUTHORITY#*\]}"
    PUBLIC_URL_PORT="${PUBLIC_URL_AFTER_BRACKET#:}"
    if [[ "${PUBLIC_URL_AUTHORITY}" == "[]"* ]]; then
      fail "--public-url 的 IPv6 方括号里没有地址（正确示例：--public-url 'http://[::1]:8790'）"
    fi
    if [[ -n "${PUBLIC_URL_AFTER_BRACKET}" ]]; then
      if [[ "${PUBLIC_URL_AFTER_BRACKET}" != :* || -z "${PUBLIC_URL_PORT}" || "${PUBLIC_URL_PORT}" == *[!0-9]* ]]; then
        fail "--public-url 的方括号后只能跟端口（正确示例：--public-url 'http://[::1]:8790'）"
      fi
    fi
  elif [[ "${PUBLIC_URL_AUTHORITY}" == *:*:* ]]; then
    # 多个冒号又不带方括号 = 裸 IPv6 字面量：master 的 url.Parse 会放行，
    # 但拼出来的客户端地址浏览器/播放器解析不了，这里直接拦下并给出正确写法。
    fail "--public-url 的 IPv6 地址必须加方括号（正确示例：--public-url 'http://[::1]:8790'）"
  fi
fi

# --- 沙盒（仅自动化测试用）---------------------------------------------------
INSTALL_ROOT="${GD_AGENT_INSTALL_ROOT:-}"
SANDBOX=0
if [[ -n "${INSTALL_ROOT}" && "${INSTALL_ROOT}" != "/" ]]; then
  SANDBOX=1
  INSTALL_ROOT="${INSTALL_ROOT%/}"
  CONFIG_DIR="${INSTALL_ROOT}${CONFIG_DIR}"
  CONFIG_FILE="${INSTALL_ROOT}${CONFIG_FILE}"
  BIN_PATH="${INSTALL_ROOT}${BIN_PATH}"
  UNIT_PATH="${INSTALL_ROOT}${UNIT_PATH}"
fi

if [[ "${SANDBOX}" -eq 0 && "$(id -u)" -ne 0 ]]; then
  fail "需要 root 权限（安装到 /usr/local/bin、建系统用户并写 systemd 单元）。请用 sudo 执行。"
fi

# --- 架构 -------------------------------------------------------------------
case "$(uname -m)" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *) fail "不支持的机器架构：$(uname -m)（只提供 linux/amd64 与 linux/arm64 两种二进制）" ;;
esac
ASSET="${ASSET_PREFIX}-${ARCH}"

if [[ -z "${DOWNLOAD_BASE}" ]]; then
  if [[ "${GITHUB_REPO}" == "OWNER/REPO" ]]; then
    fail "还不知道从哪里下载 agent 二进制：仓库 slug 仍是占位 OWNER/REPO。
  请用 --download-base <地址> 指定下载源，或设置环境变量 GD_AGENT_GITHUB_REPO=<owner>/<repo>。"
  fi
  DOWNLOAD_BASE="${GITHUB_LATEST_BASE}"
fi

# --- 下载 -------------------------------------------------------------------
WORK_DIR="$(mktemp -d)"
cleanup() { rm -rf "${WORK_DIR}"; }
trap cleanup EXIT

fetch() {
  # $1 = 远端名，$2 = 落地路径。支持 https、本地目录与 file:// 两种源。
  # ⚠️ 三行必须分开写：bash 会先把 `local a=.. b=..` 里所有参数**全部展开**
  # 再赋值，写成一行时 `${name}` 会以未定义的身份撞上 `set -u`。
  local name="$1"
  local dest="$2"
  local source="${DOWNLOAD_BASE%/}/${name}"
  case "${source}" in
    http://* | https://*)
      curl -fsSL --max-time 300 "${source}" -o "${dest}" ||
        fail "下载失败：${source}（检查网络与 --download-base；master 地址与注册 Token 无关此项）"
      ;;
    file://*) copy_local "${source#file://}" "${dest}" ;;
    *) copy_local "${source}" "${dest}" ;;
  esac
}

copy_local() {
  local src="$1" dest="$2"
  [[ -f "${src}" ]] || fail "本地下载源里没有这个文件：${src}"
  cp "${src}" "${dest}"
}

log "下载 ${ASSET}（下载源：${DOWNLOAD_BASE%/}/）"
fetch "${ASSET}" "${WORK_DIR}/${ASSET}"
fetch "${CHECKSUMS_ASSET}" "${WORK_DIR}/${CHECKSUMS_ASSET}"

# 校验必须真的发生：checksums.txt 里没有这一行就等于没校验，宁可拒绝安装。
CHECK_LINE="${WORK_DIR}/checksums.filtered"
grep -E "[[:space:]]${ASSET}\$" "${WORK_DIR}/${CHECKSUMS_ASSET}" >"${CHECK_LINE}" ||
  fail "校验文件里没有 ${ASSET} 的条目，拒绝安装未经校验的二进制"
(cd "${WORK_DIR}" && sha256sum -c "${CHECK_LINE}" >/dev/null) ||
  fail "${ASSET} 的 sha256 与校验文件不符（下载被篡改或资产名对不上，已中止安装）"
log "sha256 校验通过"

# --- 安装二进制 --------------------------------------------------------------
install -d -m 755 "$(dirname "${BIN_PATH}")"
install -m 755 "${WORK_DIR}/${ASSET}" "${BIN_PATH}"
log "已安装二进制：${BIN_PATH}（$("${BIN_PATH}" version 2>/dev/null || echo "版本读取失败")）"

# --- 服务用户 ---------------------------------------------------------------
if [[ "${SANDBOX}" -eq 1 ]]; then
  # 沙盒里不建系统用户（那是全局状态）；用当前用户冒充服务用户，
  # 让下面那条 chown 也真的被执行到——它正是最容易漏、漏了必现 EACCES 的一步。
  SERVICE_USER="$(id -un)"
else
  if ! getent group "${SERVICE_USER}" >/dev/null 2>&1; then
    groupadd --system "${SERVICE_USER}"
  fi
  if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    useradd --system --gid "${SERVICE_USER}" --home-dir "/var/lib/${SERVICE_USER}" \
      --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}"
  fi
fi
install -d -m 755 "${CONFIG_DIR}"

# --- 写 systemd 单元（幂等：内容变化才覆盖）---------------------------------
UNIT_CONTENT="$(
  cat <<UNIT
[Unit]
Description=gd-agent —— GD 代理网络节点
Documentation=https://github.com/${GITHUB_REPO}
# network-online.target：服务起来就要连 master 注册/心跳，网络没通会白白失败一轮。
After=network-online.target
Wants=network-online.target
# 反复失败时不要无限重启刷日志：5 分钟 10 次就放弃，留下现场供排查。
# （这两个键属于 [Unit]：systemd 230 起它们从 [Service] 挪到了这里。）
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
ExecStart=${BIN_PATH} serve
# 配置里含 agent_secret / sign_key（0600），不经环境变量下发。
Restart=always
RestartSec=5
# 只需要出站（连 master 与 Google）与监听端口，不给任何额外特权。
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
)"

UNIT_CHANGED=0
if [[ "${SANDBOX}" -eq 1 ]]; then
  # 沙盒里那棵树是新建的：systemd 单元目录还不存在，先建出来（同样是为了让
  # 「写单元」这一步也被冒烟测试真的走到）。
  install -d -m 755 "$(dirname "${UNIT_PATH}")"
  printf '%s\n' "${UNIT_CONTENT}" >"${UNIT_PATH}"
  UNIT_CHANGED=1
elif [[ -d "$(dirname "${UNIT_PATH}")" ]]; then
  # 只在内容真的变化时覆盖：免得每次重跑脚本都改一次文件的 mtime。
  if [[ ! -f "${UNIT_PATH}" ]] || [[ "$(cat "${UNIT_PATH}")" != "${UNIT_CONTENT}" ]]; then
    printf '%s\n' "${UNIT_CONTENT}" >"${UNIT_PATH}"
    chmod 644 "${UNIT_PATH}"
    UNIT_CHANGED=1
  fi
else
  fail "找不到 systemd 单元目录 $(dirname "${UNIT_PATH}")：本脚本要求带 systemd 的 Linux 发行版"
fi

# --- 注册（幂等）------------------------------------------------------------
NEED_ENROLL=1
if [[ -f "${CONFIG_FILE}" && "${FORCE_ENROLL}" -eq 0 ]]; then
  NEED_ENROLL=0
  log "已存在配置 ${CONFIG_FILE}：按「只升级二进制」处理，不重新注册（要重新注册请加 --force-enroll）"
fi

if [[ "${NEED_ENROLL}" -eq 1 ]]; then
  if [[ -z "${MASTER}" ]]; then
    fail "缺少 --master（首次安装必须给出 master 地址）"
  fi
  if [[ -z "${TOKEN}" ]]; then
    fail "缺少 --token（首次安装必须给出注册 Token：master 网页「节点」页可生成）"
  fi
  log "注册到 master：${MASTER}"
  ENROLL_ARGS=(enroll --master "${MASTER}" --token "${TOKEN}" --port "${PORT}" --config "${CONFIG_FILE}")
  if [[ -n "${PUBLIC_URL}" ]]; then
    ENROLL_ARGS+=(--public-url "${PUBLIC_URL}")
  fi
  if ! "${BIN_PATH}" "${ENROLL_ARGS[@]}"; then
    fail "注册失败（master 未接受这次注册）。常见原因：
  · 注册 Token 不对或已被轮换——到 master 网页「节点」页重新生成一条安装命令；
  · master 地址写错或从本机不可达——curl -fsS ${MASTER%/}/healthz 可自检；
  · 系统时间偏差过大——HTTPS 握手会因此失败。"
  fi
  # ⚠️ 关键一步：enroll 以 root 写出 0600 的 root:root 文件，而服务以
  # SERVICE_USER 运行。不交给服务用户 = 服务每次启动都 EACCES（现象是「一直离线」）。
  chown "${SERVICE_USER}:${SERVICE_USER}" "${CONFIG_FILE}"
  chmod 600 "${CONFIG_FILE}"
  log "已把配置交给服务用户 ${SERVICE_USER}（0600，仅属主可读）"
fi

if [[ ! -f "${CONFIG_FILE}" ]]; then
  fail "配置文件 ${CONFIG_FILE} 不存在：注册未成功完成，无法启动服务"
fi

# 升级路径的 --public-url：不重新注册（凭据不轮换），只就地改 config.env 的
# PUBLIC_BASE_URL 行；下面的启动段会重启服务，改动立即生效。
if [[ "${NEED_ENROLL}" -eq 0 && -n "${PUBLIC_URL}" ]]; then
  update_public_url "${CONFIG_FILE}" "${PUBLIC_URL}"
  log "已更新对外地址：PUBLIC_BASE_URL=${PUBLIC_URL}（服务重启后生效）"
fi

# 幂等路径（配置早就存在）里也要保证属主正确——旧版本脚本装过的机器上，
# 这个文件很可能还是 root:root（这正是那次 EACCES 的成因）。
# 沙盒里 SERVICE_USER 就是当前用户，chown 是空操作，但**照样执行**：
# 于是这条修复路径在沙盒测试里也被真的走到（installshell_sandbox_internal_test.go）。
chown "${SERVICE_USER}:${SERVICE_USER}" "${CONFIG_FILE}"
chmod 600 "${CONFIG_FILE}"

# --- 启动 -------------------------------------------------------------------
if [[ "${SANDBOX}" -eq 1 ]]; then
  log "沙盒模式（GD_AGENT_INSTALL_ROOT=${INSTALL_ROOT}）：跳过 systemd 操作，安装流程到此完成"
  exit 0
fi

systemctl daemon-reload
if systemctl is-enabled --quiet "${SERVICE_NAME}" 2>/dev/null; then
  systemctl restart "${SERVICE_NAME}"
else
  systemctl enable --now "${SERVICE_NAME}"
fi
if [[ "${UNIT_CHANGED}" -eq 1 ]]; then
  log "systemd 单元已更新：${UNIT_PATH}"
fi

log "完成。服务状态：systemctl status ${SERVICE_NAME}"
log "看日志：journalctl -u ${SERVICE_NAME} -f"
log "回到 master 网页「节点」页：节点会在第一次心跳（15 秒内）后出现在线；"
log "NAT 后的机器若显示不出地址，请带 --public-url <本机对外地址> 重跑本脚本（就地更新配置并重启，无需 --force-enroll）。"
