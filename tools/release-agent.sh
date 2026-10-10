#!/usr/bin/env bash
# gd-agent 本机构建与发布（v0.4.2 起；替代 GitHub Actions 流水线）
#
# 用法：
#   tools/release-agent.sh <版本号> [--repo owner/repo] [--skip-tests]
#   例：tools/release-agent.sh 0.4.2 --repo yating1022/<发布仓库>
#
# 凭证：环境变量 GH_TOKEN 或 ~/.config/gd-release/token（0600）。
#   细粒度 Token 权限：仅对发布仓库的 Contents = Read and write。
#
# 做了什么：
#   1. 质量门：go vet + go test -race（agent 模块，--skip-tests 可跳过）
#   2. 交叉编译 linux/amd64 + linux/arm64（注入版本号，静态链接）
#   3. 生成 checksums.txt（sha256，格式与安装脚本逐字兼容）
#   4. 在发布仓库创建 Release（tag: agent-v<版本>）并上传三件套
#   5. 验证 Releases/latest 资产可匿名下载
#
# 资产名与 agent-install.sh 的 GITHUB_LATEST_BASE 约定逐字一致：
#   gd-agent-linux-amd64 / gd-agent-linux-arm64 / checksums.txt
# 源码不离开本机；本脚本不在发布仓库放任何源文件。
set -euo pipefail

REPO="${GH_RELEASE_REPO:-}"
VERSION=""
SKIP_TESTS=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) REPO="${2:-}"; shift 2 ;;
    --skip-tests) SKIP_TESTS=1; shift ;;
    -h | --help) sed -n '2,20p' "$0"; exit 0 ;;
    *) VERSION="$1"; shift ;;
  esac
done

[[ -n "${VERSION}" ]] || { echo "用法：$0 <版本号> [--repo owner/repo] [--skip-tests]" >&2; exit 1; }
[[ -n "${REPO}" ]] || { echo "缺少发布仓库：--repo owner/repo 或 GH_RELEASE_REPO" >&2; exit 1; }
TAG="agent-v${VERSION}"

TOKEN="${GH_TOKEN:-}"
if [[ -z "${TOKEN}" && -f "${HOME}/.config/gd-release/token" ]]; then
  TOKEN="$(tr -d '[:space:]' <"${HOME}/.config/gd-release/token")"
fi
[[ -n "${TOKEN}" ]] || {
  echo "缺少 GitHub Token：设置 GH_TOKEN，或把 Token 写入 ~/.config/gd-release/token（0600）" >&2
  exit 1
}

export PATH="$PATH:/usr/local/go/bin"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
API="https://api.github.com/repos/${REPO}"

echo "== 预检发布仓库 ${REPO}"
REPO_JSON="$(curl -fsS -H "Authorization: Bearer ${TOKEN}" -H "Accept: application/vnd.github+json" "${API}")"
printf '%s' "${REPO_JSON}" | python3 -c '
import json, sys
d = json.load(sys.stdin)
if d.get("private"):
    sys.exit("发布仓库是私有的：匿名节点无法下载 Release 资产，请改为公开仓库")
if d.get("default_branch") is None:
    sys.exit("发布仓库还是空的：先在网页上创建一次初始提交（勾选 Add a README）再发版")
print("  仓库就绪：公开、默认分支 =", d["default_branch"])
'

cd "${ROOT}/agent"

if (( SKIP_TESTS == 0 )); then
  echo "== 质量门：go vet + go test -race"
  go vet ./...
  go test -race ./...
fi

echo "== 交叉编译 v${VERSION}"
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "dist/gd-agent-linux-${arch}" .
done
file dist/gd-agent-linux-amd64 dist/gd-agent-linux-arm64
./dist/gd-agent-linux-amd64 version
./dist/gd-agent-linux-amd64 help >/dev/null

echo "== 生成校验和"
(cd dist && sha256sum gd-agent-linux-amd64 gd-agent-linux-arm64 > checksums.txt && cat checksums.txt)

echo "== 创建 Release ${TAG}"
REL_JSON="$(curl -fsS -X POST "${API}/releases" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  -d "{\"tag_name\":\"${TAG}\",\"name\":\"gd-agent ${TAG}\",\"body\":\"agent 节点二进制（linux/amd64、linux/arm64，静态链接，零第三方依赖）与 checksums.txt。安装：在 master 网页「节点」页复制一键命令执行。\"}")"
REL_ID="$(printf '%s' "${REL_JSON}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
echo "  release id = ${REL_ID}"

for f in gd-agent-linux-amd64 gd-agent-linux-arm64 checksums.txt; do
  echo "== 上传 ${f}"
  curl -fsS -X POST "https://uploads.github.com/repos/${REPO}/releases/${REL_ID}/assets?name=${f}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/octet-stream" \
    --data-binary "@dist/${f}" >/dev/null
done

echo "== 验证 Releases/latest 资产"
sleep 2
curl -fsS -H "Accept: application/vnd.github+json" "${API}/releases/latest" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("  latest =", d["tag_name"])
for a in d["assets"]:
    print("  -", a["name"], a["size"], "字节")
'
echo "完成：${TAG} 已发布（源码未离开本机）"
