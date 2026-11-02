#!/bin/sh
# FamilyPrint「家印」一键下载脚本
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/mohan9898/familyprint/main/get.sh | sh
#   # 国内 raw 也慢的话，用镜像：
#   # curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mohan9898/familyprint/main/get.sh | sh
#
# 可选参数（通过环境变量传给 sh）：
#   INSTALL_DIR  安装目录，默认 /usr/local/bin（无权限则自动用 ~/.local/bin）
#   ONLY         只下载一部分：cloud / agent / both（默认按当前设备自动判断）
#   VERSION      指定版本，默认最新版
#   MIRROR       指定加速前缀，默认自动选择最快的
#   NO_VERIFY    1=跳过 SHA256 校验（不推荐）
#
# 例：只要家庭端到指定目录
#   curl -fsSL .../get.sh | INSTALL_DIR=$PWD ONLY=agent sh

set -eu

REPO="mohan9898/familyprint"
INSTALL_DIR="${INSTALL_DIR:-}"
ONLY="${ONLY:-auto}"
VERSION="${VERSION:-latest}"
MIRROR="${MIRROR:-auto}"
NO_VERIFY="${NO_VERIFY:-0}"

# 国内加速前缀（按实测速度排序，失败自动切换；最后兜底直连）
MIRRORS="
https://ghfast.top/
https://gh-proxy.com/
https://ghproxy.net/
"

say() { printf '%s\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
err() { printf '\033[31m%s\033[0m\n' "$*" >&2; }
ok() { printf '\033[32m%s\033[0m\n' "$*"; }

# ---------- 1. 识别架构 ----------
detect_arch() {
	os=$(uname -s | tr 'A-Z' 'a-z')
	m=$(uname -m)
	case "$m" in
		x86_64 | amd64) a=amd64 ;;
		aarch64 | arm64) a=arm64 ;;
		armv7l | armv7 | armhf) a=arm ;;
		armv6l) a=arm ;;
		i386 | i686) err "不支持 32 位 x86"; exit 1 ;;
		*) err "无法识别架构: $m（支持 x86_64 / aarch64 / armv7）"; exit 1 ;;
	esac
	case "$os" in
		linux) o=linux ;;
		darwin) o=darwin ;;
		*) err "不支持的系统: $os（Windows 请手动下载 .exe，见 Releases 页）"; exit 1 ;;
	esac
	printf '%s-%s' "$o" "$a"
}

# ---------- 2. 选择要下载的组件 ----------
# 默认策略：本机看起来像服务器（有公网 IP / 是 VPS）就下云端，否则下家庭端；
# 拿不准就两个都下，反正每个才 5-9MB。
detect_role() {
	if [ "$ONLY" != "auto" ]; then
		printf '%s' "$ONLY"
		return
	fi
	printf 'both'
}

# ---------- 3. 下载（多镜像自动重试） ----------
# 注意：这里不能用 while 读管道（子 shell 里 exit 无法传回），改成遍历变量 + 标志位。
fetch() {
	url_path="$1"    # 例如 /releases/download/v1.1.3/fp-agent-linux-amd64
	out="$2"
	min_size="${3:-1000000}"   # 小于该字节数视为下载异常（校验文件传小值）

	if [ "$MIRROR" = "auto" ]; then
		list=$(printf '%s' "$MIRRORS")
		finish=""   # 末尾空项 = 直连兜底
	else
		list=$(printf '%s' "$MIRROR" | tr ',' '\n')
		finish="__none__"
	fi

	IFS='
'
	for base in $list; do
		case "$base" in
			"") target="https://github.com$url_path" ; label="直连 GitHub" ;;
			*) target="${base}https://github.com$url_path" ; label="$base" ;;
		esac
		IFS='
'
		printf '  下载中… (%s)\n' "$label"
		if curl -fL --connect-timeout 10 --max-time 300 --retry 1 -# -o "$out" "$target" 2>/dev/null; then
			sz=$(wc -c <"$out" 2>/dev/null | tr -d ' ')
			if [ "${sz:-0}" -ge "$min_size" ]; then
				ok "  ✓ 下载完成（$(awk -v s="$sz" 'BEGIN{printf "%.1f", s/1048576}') MB）"
				return 0
			fi
			warn "  ✗ 文件异常（${sz}B < ${min_size}B），换下一个镜像"
			rm -f "$out"
		else
			warn "  ✗ 该镜像不可用，换下一个"
			rm -f "$out"
		fi
	done

	# 自动模式下最后用直连兜底
	if [ "$MIRROR" = "auto" ]; then
		target="https://github.com$url_path"
		printf '  下载中… (直连 GitHub)\n'
		if curl -fL --connect-timeout 10 --max-time 300 --retry 1 -# -o "$out" "$target" 2>/dev/null; then
			sz=$(wc -c <"$out" 2>/dev/null | tr -d ' ')
			if [ "${sz:-0}" -ge "$min_size" ]; then
				ok "  ✓ 下载完成（$(awk -v s="$sz" 'BEGIN{printf "%.1f", s/1048576}') MB）"
				return 0
			fi
		fi
		rm -f "$out"
	fi
	return 1
}

# ---------- 主流程 ----------
say ""
say "FamilyPrint「家印」一键下载"
say "----------------------------------------"

if ! command -v curl >/dev/null 2>&1; then
	err "缺少 curl，请先安装："
	say "  Debian/Ubuntu: apt install -y curl"
	say "  OpenWrt:       opkg update && opkg install curl ca-bundle"
	err "如果系统只有 wget，可改用："
	say "  wget -O fp-agent https://ghfast.top/https://github.com/$REPO/releases/latest/download/fp-agent-$(detect_arch) && chmod +x fp-agent"
	exit 1
fi

PLAT=$(detect_arch)
ROLE=$(detect_role)
say "检测到：$(uname -s) / $(uname -m)  →  $PLAT"
say ""

if [ "$VERSION" = "latest" ]; then
	BASE="/$REPO/releases/latest/download"
else
	BASE="/$REPO/releases/download/$VERSION"
fi

# 确定安装目录（无权限时退回用户目录，不打扰用户输密码）
if [ -z "$INSTALL_DIR" ]; then
	if [ -w /usr/local/bin ]; then
		INSTALL_DIR=/usr/local/bin
	else
		mkdir -p "$HOME/.local/bin"
		INSTALL_DIR="$HOME/.local/bin"
		warn "当前用户对 /usr/local/bin 无写权限，安装到 $INSTALL_DIR"
		case ":$PATH:" in
			*":$INSTALL_DIR:"*) ;;
			*) warn "提示：请把 $INSTALL_DIR 加入 PATH（或重新登录后再执行）" ;;
		esac
	fi
fi
mkdir -p "$INSTALL_DIR"

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t fp)
trap 'rm -rf "$TMP"' EXIT INT TERM

need_cloud=0
need_agent=0
case "$ROLE" in
	cloud) need_cloud=1 ;;
	agent) need_agent=1 ;;
	both) need_cloud=1; need_agent=1 ;;
	*) err "ONLY 只能是 cloud / agent / both"; exit 1 ;;
esac

# 校验文件（一次下完，统一校验；文件很小，阈值放到 1KB）
say "1/3 下载校验文件"
if fetch "$BASE/sha256sums.txt" "$TMP/sha256sums.txt" 1000; then
	HAVE_SUMS=1
else
	HAVE_SUMS=0
	warn "校验文件下载失败，将跳过 SHA256 校验"
fi
say ""

install_one() {
	name="$1"     # fp-cloud / fp-agent
	title="$2"
	say "2/3 下载 $title（$name-$PLAT）"
	if ! fetch "$BASE/$name-$PLAT" "$TMP/$name"; then
		err "所有镜像都失败了。可尝试："
		say "  1) 换个镜像：MIRROR=https://gh-proxy.com/ sh"
		say "  2) 手动下载：https://github.com/$REPO/releases"
		exit 1
	fi

	if [ "$HAVE_SUMS" = "1" ] && [ "$NO_VERIFY" != "1" ]; then
		expect=$(grep " $name-$PLAT\$" "$TMP/sha256sums.txt" 2>/dev/null | awk '{print $1}' | head -1)
		if [ -n "$expect" ]; then
			actual=$(sha256sum "$TMP/$name" 2>/dev/null | awk '{print $1}')
			[ -z "$actual" ] && actual=$(shasum -a 256 "$TMP/$name" | awk '{print $1}')
			if [ "$expect" != "$actual" ]; then
				err "SHA256 校验失败！文件可能被篡改或下载不完整。"
				say "  期望: $expect"
				say "  实际: $actual"
				exit 1
			fi
			ok "  ✓ SHA256 校验通过"
		fi
	fi

	chmod +x "$TMP/$name"
	cp "$TMP/$name" "$INSTALL_DIR/$name"
	ok "  ✓ 已安装到 $INSTALL_DIR/$name"
	say ""
}

if [ "$need_cloud" = "1" ]; then install_one fp-cloud "云端服务端"; fi
if [ "$need_agent" = "1" ]; then install_one fp-agent "家庭端客户端"; fi

# ---------- 收尾：打印下一步怎么用 ----------
say "3/3 完成"
say "----------------------------------------"
ok "✅ 安装完成"
say ""
if [ "$need_cloud" = "1" ]; then
	say "【云端】VPS 上的一条命令启动（把两个 xxx 换成随机字符）："
	say "  fp-cloud -secret xxx -token xxx -data /var/lib/familyprint -listen 127.0.0.1:8000"
	say ""
fi
if [ "$need_agent" = "1" ]; then
	say "【家庭端】一条命令启动（把 xxx 换成你的值）："
	say "  fp-agent -cloud wss://你的域名/ws/agent -token xxx -printer ipp://192.168.1.100:631/ipp/print"
	say ""
fi
say "查看版本：fp-cloud -h  /  fp-agent -h"
say "完整教程：https://github.com/$REPO#readme"
say ""
case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		warn "⚠️  $INSTALL_DIR 不在 PATH 中，直接输命令会提示 command not found，请执行："
		say "  export PATH=\"$INSTALL_DIR:\$PATH\""
		say "  # 想永久生效（以 ash/bash 为例）：echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.profile"
		say ""
		;;
esac
