#!/bin/sh
# Vultro 离线下载引擎安装脚本（无需 Docker）。
#
#   curl -fsSL https://raw.githubusercontent.com/atticus6/vultro-engine/main/install.sh | sh
#
# 可选参数：
#   --version v1.2.3   指定版本（默认取最新 release）
#   --dir /path        安装目录（默认 /usr/local/bin；没有写权限时自动改 ~/.local/bin）
#
# 设计要点：
# - **只做一件事**：下载对应平台的 release 归档并解出二进制。不做 systemd、不写配置——
#   环境变量怎么给（systemd unit / shell profile / compose）是部署形态的事。
# - 校验和来自同一个 release 的 checksums.txt（没有它就不装：一个被篡改的二进制比没装更糟）。
set -eu

REPO="atticus6/vultro-engine"
BIN="vultro-engine"
VERSION=""
DIR=""

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version 需要一个值}"; shift 2 ;;
    --dir) DIR="${2:?--dir 需要一个值}"; shift 2 ;;
    -h|--help) sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
echo "用 'vultro-engine' 启动；环境变量见 $REPO 的 README。"
