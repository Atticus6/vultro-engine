#!/bin/sh
# Vultro 离线下载引擎安装脚本（无需 Docker）。
#
#   curl -fsSL https://raw.githubusercontent.com/atticus6/vultro-engine/main/install.sh | sh -s -- [参数]
#
# 安装参数（只影响"装哪个版本、装到哪"）：
#   --version v1.2.3   指定版本（默认取最新 release）
#   --dir /path        安装目录（默认 /usr/local/bin；没有写权限时自动改 ~/.local/bin）
#
# 引擎配置参数（写进配置文件，供引擎启动时读取）：
#   --engine-id <id>   节点 id，如 off1
#   --token <eng_…>    该节点专属令牌
#   --worker-url <url> Worker 基址
#   --capacity <1-16>  并发槽位
#   --egress <mode>    relay / warp / direct
#   --config <path>    配置文件路径（默认 ~/.config/vultro-engine/config）
#   --no-config        只装二进制，不写配置文件
#
# 优先级：**命令行参数 > 环境变量 > 配置文件**。这里写的只是"落盘默认值"，
# 之后用 `vultro-engine --token=…` 或环境变量都能临时盖掉，不必改文件。
#
# 设计要点：
# - **只做两件事**：下载对应平台的 release 归档并解出二进制；把给定配置写进
#   配置文件（权限 600，里面有令牌）。不做 systemd、不启动服务——部署形态的事
#   交给部署方。
# - 校验和来自同一个 release 的 checksums.txt（没有它就不装：一个被篡改的二进制比没装更糟）。
set -eu

REPO="atticus6/vultro-engine"
BIN="vultro-engine"
VERSION=""
DIR=""
WRITE_CONFIG=1

# 要写进配置文件的键值（空 = 不写这一项）。
CFG_ENGINE_ID=""
CFG_TOKEN=""
CFG_WORKER_URL=""
CFG_CAPACITY=""
CFG_EGRESS=""
CONFIG_PATH=""

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version 需要一个值}"; shift 2 ;;
    --dir) DIR="${2:?--dir 需要一个值}"; shift 2 ;;
    --engine-id) CFG_ENGINE_ID="${2:?--engine-id 需要一个值}"; shift 2 ;;
    --token) CFG_TOKEN="${2:?--token 需要一个值}"; shift 2 ;;
    --worker-url) CFG_WORKER_URL="${2:?--worker-url 需要一个值}"; shift 2 ;;
    --capacity) CFG_CAPACITY="${2:?--capacity 需要一个值}"; shift 2 ;;
    --egress) CFG_EGRESS="${2:?--egress 需要一个值}"; shift 2 ;;
    --config) CONFIG_PATH="${2:?--config 需要一个值}"; shift 2 ;;
    --no-config) WRITE_CONFIG=0; shift ;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$VERSION" ]; then
  # 拿最新 tag。用 releases/latest 的 302 Location（不含 API 限流，也不需要 jq）。
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')
  [ -n "$VERSION" ] || { echo "无法获取最新版本" >&2; exit 1; }
fi

# 归档名里的版本不带 v 前缀（goreleaser 的 .Version 语义）。
PLAIN_VERSION="${VERSION#v}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux|darwin) ;;
  *) echo "不支持的系统: $OS" >&2; exit 1 ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "不支持的架构: $ARCH" >&2; exit 1 ;;
esac

ASSET="${BIN}_${PLAIN_VERSION}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "下载 $ASSET …"
curl -fsSL "$BASE/$ASSET" -o "$TMP/$ASSET"
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"

echo "校验 …"
( cd "$TMP" && grep " $ASSET\$" checksums.txt | sha256sum -c - >/dev/null ) || {
  # macOS 的 sha256sum 不存在；`shasum -a 256` 是等价物。
  EXPECTED=$(grep " $ASSET\$" "$TMP/checksums.txt" | awk '{print $1}')
  ACTUAL=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
  [ "$EXPECTED" = "$ACTUAL" ] || { echo "校验和不符，已中止" >&2; exit 1; }
}

tar -xzf "$TMP/$ASSET" -C "$TMP"

if [ -z "$DIR" ]; then
  if [ -w /usr/local/bin ]; then
    DIR=/usr/local/bin
  else
    DIR="$HOME/.local/bin"
    mkdir -p "$DIR"
    case ":$PATH:" in
      *":$DIR:"*) ;;
      *) echo "提示：$DIR 不在 PATH 中，请把它加进 shell 配置。" ;;
    esac
  fi
fi

install -m 0755 "$TMP/$BIN" "$DIR/$BIN"
echo "已安装 $DIR/$BIN（$VERSION）"

# ── 写配置文件 ────────────────────────────────────────────────────────────────
#
# 只写用户显式给了值的项：没给的留空，引擎按环境变量与内置默认继续回退。
# 权限 600——里面有节点令牌。已存在的文件会被覆盖，但先备份一份，避免手改的内容
# 悄悄消失（管理员常在里面加注释或别的变量）。
if [ "$WRITE_CONFIG" -eq 1 ]; then
  if [ -z "$CONFIG_PATH" ]; then
    if [ -n "${XDG_CONFIG_HOME:-}" ]; then
      CONFIG_DIR="$XDG_CONFIG_HOME/vultro-engine"
    else
      CONFIG_DIR="$HOME/.config/vultro-engine"
    fi
    CONFIG_PATH="$CONFIG_DIR/config"
  else
    CONFIG_DIR=$(dirname "$CONFIG_PATH")
  fi

  mkdir -p "$CONFIG_DIR"
  if [ -f "$CONFIG_PATH" ]; then
    cp "$CONFIG_PATH" "$CONFIG_PATH.bak"
    echo "已备份原配置到 $CONFIG_PATH.bak"
  fi

  # 用 umask 保证文件从创建起就是 600（先 touch 再 chmod 会有一瞬间是 644）。
  #
  # 每个键都用 `if` 而不是 `[ -n … ] && echo`：脚本是 `set -e`，后者在值为空时
  # 返回 1，会让整个安装在这里静默中止（而且前面已经把二进制装好了，看起来像成功）。
  (
    umask 077
    {
      echo "# vultro-engine 配置（install.sh 生成于 $(date -u +%Y-%m-%dT%H:%M:%SZ)）"
      echo "# 优先级：命令行参数 > 环境变量 > 本文件"
      if [ -n "$CFG_ENGINE_ID" ]; then echo "ENGINE_ID=$CFG_ENGINE_ID"; fi
      if [ -n "$CFG_TOKEN" ]; then echo "ENGINE_TOKEN=$CFG_TOKEN"; fi
      if [ -n "$CFG_WORKER_URL" ]; then echo "WORKER_BASE_URL=$CFG_WORKER_URL"; fi
      if [ -n "$CFG_CAPACITY" ]; then echo "ENGINE_CAPACITY=$CFG_CAPACITY"; fi
      if [ -n "$CFG_EGRESS" ]; then echo "EGRESS_MODE=$CFG_EGRESS"; fi
    } > "$CONFIG_PATH"
  )

  if [ -n "$CFG_TOKEN" ] || [ -n "$CFG_ENGINE_ID" ] || [ -n "$CFG_WORKER_URL" ]; then
    echo "已写入配置 $CONFIG_PATH"
  else
    echo "未提供引擎配置参数，$CONFIG_PATH 只含注释（可用环境变量启动）"
  fi
  echo "查看生效配置：$BIN --print-config"
fi

echo "用 'vultro-engine' 启动；其余变量见 $REPO 的 README。"
