<div align="center">

# FamilyPrint「家印」

**全家人共用的远程打印服务 · 微信里打开网址就能打印 · 老人零学习成本**

[![License](https://img.shields.io/badge/License-PolyForm_NC_1.0.0-orange)](./LICENSE)
[![Release](https://img.shields.io/github/v/release/mohan9898/familyprint)](https://github.com/mohan9898/familyprint/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/mohan9898/familyprint/ci.yml?branch=main&label=CI)](https://github.com/mohan9898/familyprint/actions)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Built by AI](https://img.shields.io/badge/Built_by-100%25_AI-8A2BE2)](#-纯-ai-编写声明)

</div>

---

## 目录

| 章节 | 内容 | 适合谁看 |
|------|------|---------|
| [一](#一这是个什么项目) | 项目介绍、原理图 | 所有人 |
| [二](#二开始前的准备) | 准备工作清单、生成密钥 | 所有人 |
| [三](#三部署方式怎么选) | **部署方式怎么选（先看这个）** | 所有人 |
| [四](#四方案一docker-一键部署推荐) | **方案一：Docker 一键部署（推荐）** | 新手首选 |
| [五](#五方案二docker-手动部署) | 方案二：Docker 手动部署 | 想了解细节 |
| [六](#六方案三二进制一键部署) | **方案三：二进制一键部署** | 不想装 Docker |
| [七](#七方案四二进制手动部署) | 方案四：二进制手动部署 | 高级用户 |
| [八](#八测试打印) | 部署完成后测试 | 所有人 |
| [九](#九管理员功能) | 网页管理后台 | 管理员 |
| [十](#十交给家人) | 给家人发使用说明 | 所有人 |
| [十一](#十一进阶端到端加密推荐开启) | 端到端加密 | 有隐私需求 |
| [十二](#十二日志怎么看) | 日志查看与排错 | 排问题时 |
| [十三](#十三日常维护) | 备份/迁移/升级 | 所有人 |
| [十四](#十四故障排查) | 常见问题对照表 | 排问题时 |
| [附录](#附录) | 参数表/架构对照/源码构建 | 按需 |

---

## 一、这是个什么项目？

**一句话**：在外用手机给家里的打印机发打印任务，家人回家取纸。

```
📱 家人手机（微信/浏览器打开网址，选文件点打印）
        ↓ 加密上传
☁️ 云端服务器（你的 VPS，负责保管账号、排队任务、中转文件）
        ↓ 自动加密通道（家庭端主动连出，路由器不用开放任何端口）
🏠 家里的软路由/NAS（fp-agent 小程序，负责把任务送到打印机）
        ↓
🖨️ 打印机出纸
```

**家里老人怎么用**：打开网址 → 选文件 → 点「开始打印」→ 回家取。完事。登录一次 30 天免输密码。

**你需要一次性搭建**（全程约 30 分钟）：

| 位置 | 装什么 | 干什么的 |
|------|--------|----------|
| 云端 VPS | fp-cloud 服务端 | 收任务、管账号（网页入口） |
| 家里常开设备 | fp-agent 客户端 | 收任务、送打印机 |

---

## 二、开始前的准备

### ✅ 你需要有的东西

| 序号 | 需要 | 检查方法 | 没有怎么办 |
|:---:|------|----------|-----------|
| ① | 一个**备案域名**（如 `print.example.com`） | 能登录域名管理后台 | 去阿里云/腾讯云买一个并备案 |
| ② | 一台**公网 VPS**（1核512M 足够） | SSH 能连上（教程会教） | 任意云厂商轻量服务器 |
| ③ | 一台**家里常开的设备**（软路由/NAS/树莓派/旧电脑） | 能开机、能上网 | — |
| ④ | 一台**网络打印机**（WiFi/网线接入家里局域网） | 打印机屏幕里能查到 IP（如 `192.168.1.100`） | USB 打印机暂不支持 |

> ❓ **什么是 SSH 登录**：就是远程命令行。Windows 按 `Win+R` 输入 `powershell` 回车，然后输 `ssh root@你的VPS_IP`，第一次输 `yes`，再输密码（输密码时屏幕不显示，正常输完回车）。Mac/Linux 自带终端直接输 `ssh` 命令。

### ✅ 生成两个「密码」（后面反复用到）

SSH 登录 VPS 后执行（生成两串随机字符，**复制保存到记事本**）：

```bash
# 第 1 串：设备令牌 TOKEN（云端和家庭端要用同一个）
openssl rand -hex 32

# 第 2 串：会话密钥 SECRET（仅云端用，再执行一次即可）
openssl rand -hex 32
```

> 💡 **记住这 4 个值**，后面每一处「必改」都指它们：
> ① 域名 ② VPS 的 IP ③ TOKEN（两串随机字符之一）④ SECRET（另一串）

### ✅ 找到打印机的 IPP 地址（家庭端要用）

1. 打印机屏幕：设置 → 网络设置 → 查看 IP（如 `192.168.1.100`）
2. 到路由器管理页，给这个 IP 绑定**静态分配**（防止 IP 变动）
3. IPP 地址 = `ipp://192.168.1.100:631/ipp/print`（把 IP 换成你的；个别品牌路径不同，试不通看[故障排查](#十四故障排查)）

---

## 三、部署方式怎么选

本项目分**云端**（VPS 上）和**家庭端**（家里设备上）两部分，**两部分都要装**，但可以用不同的方式：

| 方式 | 原理 | 适合谁 | 难度 |
|------|------|--------|------|
| **方案一：Docker 一键部署** ⭐ | 复制一条命令，自动生成配置并启动 | **新手首选**，OpenWrt/NAS 有 Docker 的 | ⭐ 最简单 |
| 方案二：Docker 手动部署 | 自己创建 yml 文件再启动 | 想完全掌控配置内容的人 | ⭐⭐ |
| **方案三：二进制一键部署** | 一条命令下载现成程序并安装 | 设备装不了/不想装 Docker（内存更省） | ⭐⭐ |
| 方案四：二进制手动部署 | 自己 wget 下载、手敲启动命令、自己配自启 | 高级用户/极客 | ⭐⭐⭐ |

> 💡 **如何判断设备能不能装 Docker**：SSH/终端里执行 `docker --version`，显示版本号 = 有 Docker（用方案一/二）；提示 command not found = 没有（用方案三/四，或先装 Docker）。
>
> 💡 **云端 VPS 推荐方案一**（省心）；**家里设备如果是软路由/NAS 且内存紧张（如 1G 以下），推荐方案三**（二进制只占 ~10MB 内存，Docker 方式整体要多占 100MB+）。
>
> 🇨🇳 **国内加速**：所有 GitHub 下载地址前加 `https://ghfast.top/` 即可，实测同一文件直连 90 秒仅 1.3MB，加速后 3 秒完整下载。

---

## 四、方案一：Docker 一键部署（推荐）

> 原理：一条命令从网上拉取官方部署脚本，自动创建配置文件、拉取镜像、启动服务。全程复制粘贴，不用编辑器。

### 4.1 云端（VPS 上）

**第 1 步：装 Docker**（已装跳过，验证方法见上）

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
docker --version    # 显示版本号 = 装好了
```

**第 2 步：一键部署**

把下面整段复制粘贴执行（`必改`的 2 处已标注，执行前先改）：

```bash
mkdir -p /opt/familyprint && cd /opt/familyprint && cat > docker-compose.cloud.yml <<'EOF'
services:
  cloud:
    image: ghcr.io/mohan9898/familyprint-cloud:latest
    container_name: fp-cloud
    restart: unless-stopped
    environment:
      - SECRET_KEY=把这里改成第2串随机字符        # ← 必改
      - DEVICE_TOKEN=把这里改成第1串随机字符      # ← 必改（和家庭端一致）
      - DATA_DIR=/app/data
      - LISTEN=:8000
      - LOG_LEVEL=info                           # 日志级别：debug/info/warn/error
    volumes:
      - cloud-data:/app/data
    ports:
      - "8000:8000"
    logging:                                     # 日志自动轮转，防占满磁盘
      driver: json-file
      options: { max-size: "10m", max-file: "3" }

volumes:
  cloud-data:
EOF
docker compose -f docker-compose.cloud.yml up -d
```

**第 3 步：配置 HTTPS 反代**

在你自己的 HTTPS 反代里，把域名 `print.example.com` 回源到 `VPS_IP:8000`（宝塔/Nginx/1Panel/云负载均衡均可，按你平时的习惯配置即可）。

**第 4 步：验证**

```bash
docker logs fp-cloud
```
看到启动横幅和 `开始监听 :8000` 就是成功：

```
FamilyPrint-Cloud 启动
  版本  v1.1.3   监听  :8000   ...
  账号/任务  1 个账号，0 条历史任务
[INF] 开始监听 :8000（Ctrl+C 停止）
```

**✅ 检查点 1**：浏览器打开 `https://print.example.com` 能看到登录页 → 登录默认账号 `admin / admin123` → **立即进「成员」改密码！**

### 4.2 家庭端（家里设备上）

家里设备装好 Docker 后（OpenWrt：网页管理界面 → 系统 → 软件包 → 搜索 `dockerd` 安装），执行：

```bash
mkdir -p /opt/familyprint && cd /opt/familyprint && cat > docker-compose.agent.yml <<'EOF'
services:
  agent:
    image: ghcr.io/mohan9898/familyprint-agent:latest
    container_name: fp-agent
    restart: unless-stopped
    network_mode: host          # 直接访问家庭网段内的打印机
    environment:
      - CLOUD_URLS=wss://print.example.com/ws/agent       # ← 必改：你的域名
      - DEVICE_TOKEN=把这里改成第1串随机字符               # ← 必改（和云端一致）
      - PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print # ← 必改：打印机地址
      - PRINTER_NAME=家里打印机
      - MOCK=0
      - DEBUG=0                                           # 调试日志（1=详细）
      - PRINT_KEY=                                        # 可选：端到端加密密钥
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
EOF
docker compose -f docker-compose.agent.yml up -d
```

**验证**：

```bash
docker logs fp-agent
```

```
[INF] FamilyPrint-Agent 启动
[INF]   云端      [wss://print.example.com/ws/agent]
[INF]   打印机    家里打印机（ipp://192.168.1.100:631/ipp/print）
[WRN] ws://... 使用未加密 ws:// 连接…（用了 wss:// 就没有这行）
[INF] 已连接云端 wss://print.example.com/ws/agent
[INF] 打印机「家里打印机」在线，探测耗时 3ms
```

**✅ 检查点 2**：回到网页打印页，顶部打印机状态显示**在线**。

> 🔄 **改了配置怎么生效**：改完 yml 文件后执行 `docker compose -f docker-compose.agent.yml up -d`（会自动重建容器）。

---

## 五、方案二：Docker 手动部署

> 和方案一的区别：不整段复制，而是一步步用编辑器创建文件，适合想逐行理解配置、或需要深度定制的人。**配置内容与方案一完全一致**，已经用方案一装好的无需重复操作。

### 5.1 云端（VPS 上）

**第 1 步：装 Docker**（SSH 登录 VPS 后执行；已装跳过）

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
docker --version    # 显示版本号 = 装好了
```

**第 2 步：建目录并创建编排文件**

```bash
mkdir -p /opt/familyprint && cd /opt/familyprint
nano docker-compose.cloud.yml
```

在编辑器里粘贴以下内容（`nano` 用法：`Ctrl+O` 回车保存，`Ctrl+X` 退出；`必改`的 2 处先改好再保存）：

```yaml
services:
  cloud:
    image: ghcr.io/mohan9898/familyprint-cloud:latest
    container_name: fp-cloud
    restart: unless-stopped
    environment:
      - SECRET_KEY=第2串随机字符          # ← 必改
      - DEVICE_TOKEN=第1串随机字符        # ← 必改（和家庭端一致）
      - DATA_DIR=/app/data
      - LISTEN=:8000
      - LOG_LEVEL=info                    # 日志级别：debug/info/warn/error
    volumes:
      - cloud-data:/app/data
    ports:
      - "8000:8000"
    logging:                              # 日志自动轮转，防占满磁盘
      driver: json-file
      options: { max-size: "10m", max-file: "3" }

volumes:
  cloud-data:
```

**第 3 步：启动**

```bash
docker compose -f docker-compose.cloud.yml up -d
```

**第 4 步：验证**

```bash
docker logs fp-cloud
```

看到启动横幅和 `开始监听 :8000` 就是成功（样例见方案一第 4 步）。

```bash
# 常用后续操作
docker compose -f docker-compose.cloud.yml stop      # 停止
docker compose -f docker-compose.cloud.yml restart   # 重启
docker compose -f docker-compose.cloud.yml down      # 停止并删除容器（数据保留在卷里）
```

**第 5 步：配置 HTTPS 反代**

在你自己的 HTTPS 反代里，把域名 `print.example.com` 回源到 `VPS_IP:8000`（宝塔/Nginx/1Panel/云负载均衡均可）。

**✅ 检查点 1**：浏览器打开 `https://print.example.com` 能看到登录页 → 登录默认账号 `admin / admin123` → **立即进「成员」改密码！**

### 5.2 家庭端（家里设备上）

家里设备装好 Docker 后（OpenWrt：网页管理界面 → 系统 → 软件包 → 搜索 `dockerd` 安装），执行：

```bash
mkdir -p /opt/familyprint && cd /opt/familyprint
nano docker-compose.agent.yml
```

粘贴以下内容：

```yaml
services:
  agent:
    image: ghcr.io/mohan9898/familyprint-agent:latest
    container_name: fp-agent
    restart: unless-stopped
    network_mode: host
    environment:
      - CLOUD_URLS=wss://print.example.com/ws/agent       # ← 必改：你的域名
      - DEVICE_TOKEN=第1串随机字符                        # ← 必改（和云端一致）
      - PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print # ← 必改：打印机地址
      - PRINTER_NAME=家里打印机
      - MOCK=0
      - DEBUG=0                                           # 调试日志（1=详细）
      - PRINT_KEY=                                        # 可选：端到端加密密钥
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
```

```bash
# 启动
docker compose -f docker-compose.agent.yml up -d

# 验证（看到「已连接云端」和「打印机在线」= 成功）
docker logs -f fp-agent    # 按 Ctrl+C 退出查看（不影响运行）
```

**✅ 检查点 2**：回到网页打印页，顶部打印机状态显示**在线**。

> 🔄 **改了配置怎么生效**：改完 yml 文件后执行 `docker compose -f docker-compose.agent.yml up -d`（会自动重建容器）。
>
> 💡 **不想用 nano**：也可以用宝塔/1Panel 的文件管理器直接创建编辑 yml 文件，然后进终端执行 `docker compose -f 文件名 up -d`。

### 5.3 直接用 docker run（不用 compose）

极简场景（比如临时测试）也可以一条 `docker run` 启动：

```bash
# 云端
docker run -d --name fp-cloud --restart unless-stopped \
  -p 8000:8000 \
  -e SECRET_KEY=第2串随机字符 \
  -e DEVICE_TOKEN=第1串随机字符 \
  -v fp-cloud-data:/app/data \
  ghcr.io/mohan9898/familyprint-cloud:latest

# 家庭端
docker run -d --name fp-agent --restart unless-stopped --network host \
  -e CLOUD_URLS=wss://print.example.com/ws/agent \
  -e DEVICE_TOKEN=第1串随机字符 \
  -e PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print \
  ghcr.io/mohan9898/familyprint-agent:latest
```

---

## 六、方案三：二进制一键部署

> 原理：一条命令下载官方编译好的单个程序文件（不用装 Docker、不用装 Go），自动完成架构识别、国内加速、SHA256 校验和安装。**内存占用最小**（云端 ~15MB、家庭端 ~10MB），适合 1G 以下内存的软路由/NAS。

### 6.1 云端（VPS 上）

**第 1 步：一键下载安装 fp-cloud**（自动识别架构 + 国内加速 + SHA256 校验）

```bash
curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mohan9898/familyprint/main/get.sh | ONLY=cloud sh
```

**第 2 步：配置开机自启（systemd）**

把下面整段复制粘贴执行（`必改`的 2 处已标注，执行前先改）：

```bash
cat > /etc/systemd/system/fp-cloud.service <<'EOF'
[Unit]
Description=FamilyPrint Cloud
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fp-cloud -secret 把这里改成第2串随机字符 -token 把这里改成第1串随机字符 -data /var/lib/familyprint -listen 127.0.0.1:8000
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-cloud
journalctl -u fp-cloud -f   # 应看到 "开始监听 :8000"，按 Ctrl+C 退出查看（不影响运行）
```

**第 3 步：配置 HTTPS 反代**

在你自己的 HTTPS 反代里，把域名 `print.example.com` 回源到 `VPS_IP:8000`（宝塔/Nginx/1Panel/云负载均衡均可）。

**✅ 检查点 1**：浏览器打开 `https://print.example.com` 能看到登录页 → 登录默认账号 `admin / admin123` → **立即进「成员」改密码！**

### 6.2 家庭端（家里设备上）

**第 1 步：一键下载安装 fp-agent**

```bash
curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mohan9898/familyprint/main/get.sh | ONLY=agent sh
```

**第 2 步：配置开机自启**

**标准 Linux / NAS（systemd）**——整段复制执行（`必改`的 3 处先改好）：

```bash
cat > /etc/systemd/system/fp-agent.service <<'EOF'
[Unit]
Description=FamilyPrint Agent
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fp-agent -cloud wss://print.example.com/ws/agent -token 把这里改成第1串随机字符 -printer ipp://192.168.1.100:631/ipp/print
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-agent
journalctl -u fp-agent -f   # 应看到 "已连接云端"，按 Ctrl+C 退出查看（不影响运行）
```

**OpenWrt（procd）**——整段复制执行（`必改`的 3 处先改好）：

```bash
cat > /etc/init.d/fp-agent <<'EOF'
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
    procd_open_instance
    procd_set_param command /usr/local/bin/fp-agent -cloud wss://print.example.com/ws/agent -token 把这里改成第1串随机字符 -printer ipp://192.168.1.100:631/ipp/print
    procd_set_param respawn
    procd_close_instance
}
EOF
chmod +x /etc/init.d/fp-agent && /etc/init.d/fp-agent enable && /etc/init.d/fp-agent start
logread -e fp-agent   # 应看到 "已连接云端"
```

**✅ 检查点 2**：回到网页打印页，顶部打印机状态显示**在线**。

### 6.3 一键脚本的可选参数

| 想改什么 | 在命令前面加环境变量 |
|----------|---------------------|
| 装到别处 | `INSTALL_DIR=/opt/bin` |
| 只装家庭端 / 只装云端 / 都装 | `ONLY=agent` / `ONLY=cloud` / `ONLY=both`（默认 both） |
| 指定版本 | `VERSION=v1.1.3` |
| 指定镜像（自动的不通时） | `MIRROR=https://gh-proxy.com/` |
| 跳过校验（不推荐） | `NO_VERIFY=1` |

```bash
# 举例：只把家庭端装到当前目录
curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mohan9898/familyprint/main/get.sh | INSTALL_DIR=$PWD ONLY=agent sh
```

> 💡 设备上没有 `curl` 只有 `wget`？OpenWrt 先执行 `opkg update && opkg install curl ca-bundle`；或者参考[方案四](#七方案四二进制手动部署)手动 wget。

**✅ 检查点**：二进制方式启动后，回到网页打印页，顶部打印机状态显示**在线**。

---

## 七、方案四：二进制手动部署

> 和方案三的区别：不用脚本，自己下载、自己校验、自己写自启配置。适合想完全掌控每个环节、或脚本跑不通时的兜底。

### 7.1 下载（手动）

先确认架构：`uname -m`（`x86_64`=amd64，`aarch64`=arm64，`armv7l`=arm），然后：

```bash
# 云端（VPS 一般是 amd64）
wget https://ghfast.top/https://github.com/mohan9898/familyprint/releases/latest/download/fp-cloud-linux-amd64 -O /usr/local/bin/fp-cloud
chmod +x /usr/local/bin/fp-cloud

# 家庭端（按实际架构换文件名，对照表见附录 B）
wget https://ghfast.top/https://github.com/mohan9898/familyprint/releases/latest/download/fp-agent-linux-arm64 -O /usr/local/bin/fp-agent
chmod +x /usr/local/bin/fp-agent
```

> 🇨🇳 上面已带加速前缀；直连就删掉 `https://ghfast.top/`。
> 🍎 Mac / 🪟 Windows 用户去 [Releases 页](https://github.com/mohan9898/familyprint/releases) 下载对应 `darwin-*` / `*-windows-amd64.exe` 文件。

**（可选但推荐）手动校验完整性**：

```bash
wget https://ghfast.top/https://github.com/mohan9898/familyprint/releases/latest/download/sha256sums.txt
sha256sum -c <(grep "fp-agent-linux-arm64" sha256sums.txt)   # 显示 OK = 文件完整
```

### 7.2 启动（手动）

```bash
# 云端（先创建数据目录）
mkdir -p /var/lib/familyprint
fp-cloud -secret 第2串随机字符 -token 第1串随机字符 -data /var/lib/familyprint -listen 127.0.0.1:8000

# 家庭端
fp-agent -cloud wss://print.example.com/ws/agent -token 第1串随机字符 -printer ipp://192.168.1.100:631/ipp/print
```

> ⚠️ 手动启动只在当前终端运行，**关掉终端/SSH 断开程序就停了**——所以正式使用必须配置开机自启（同时也解决了后台运行问题），配置如下。

**云端自启（systemd）**——整段复制执行（`必改`的 2 处先改好）：

```bash
cat > /etc/systemd/system/fp-cloud.service <<'EOF'
[Unit]
Description=FamilyPrint Cloud
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fp-cloud -secret 把这里改成第2串随机字符 -token 把这里改成第1串随机字符 -data /var/lib/familyprint -listen 127.0.0.1:8000
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-cloud
journalctl -u fp-cloud -f   # 应看到 "开始监听 :8000"，按 Ctrl+C 退出查看（不影响运行）
```

**家庭端自启（systemd）**——整段复制执行（`必改`的 3 处先改好）：

```bash
cat > /etc/systemd/system/fp-agent.service <<'EOF'
[Unit]
Description=FamilyPrint Agent
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fp-agent -cloud wss://print.example.com/ws/agent -token 把这里改成第1串随机字符 -printer ipp://192.168.1.100:631/ipp/print
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-agent
journalctl -u fp-agent -f   # 应看到 "已连接云端"，按 Ctrl+C 退出查看（不影响运行）
```

**家庭端自启（OpenWrt，procd）**——软路由用这段代替上面 systemd 那段（`必改`的 3 处先改好）：

```bash
cat > /etc/init.d/fp-agent <<'EOF'
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
    procd_open_instance
    procd_set_param command /usr/local/bin/fp-agent -cloud wss://print.example.com/ws/agent -token 把这里改成第1串随机字符 -printer ipp://192.168.1.100:631/ipp/print
    procd_set_param respawn
    procd_close_instance
}
EOF
chmod +x /etc/init.d/fp-agent && /etc/init.d/fp-agent enable && /etc/init.d/fp-agent start
logread -e fp-agent   # 应看到 "已连接云端"
```

**别忘了**：云端还需配置 HTTPS 反代（域名回源到 `VPS_IP:8000`，见方案一第 3 步）。

**✅ 检查点**：回到网页打印页，顶部打印机状态显示**在线**。

### 7.3 用环境变量代替命令行参数（可选）

不喜欢长命令的话，全部参数都可以用环境变量：

```bash
export SECRET_KEY=第2串随机字符 DEVICE_TOKEN=第1串随机字符
export DATA_DIR=/var/lib/familyprint LISTEN=127.0.0.1:8000
fp-cloud

# 家庭端同理
export CLOUD_URLS=wss://print.example.com/ws/agent DEVICE_TOKEN=第1串随机字符
export PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print
fp-agent
```

> ⚠️ 注意：Docker 部署时**必须**对外监听（`LISTEN=:8000`），只有二进制 + 本机反代场景才用 `127.0.0.1`。

---

## 八、测试打印

手机**关掉 WiFi 用流量**（模拟在外场景），完成下面清单：

- [ ] 打开 `https://print.example.com` → 登录
- [ ] 传一个 PDF → 点「开始打印」→ **家里打印机出纸**
- [ ] 改份数=2、开双面 → 出纸正确
- [ ] 微信里打开同一个网址 → 有提示卡 → 也能正常上传打印

## 九、管理员功能

用管理员账号登录网页，右上角会多一个「**管理**」入口，点进去就是管理后台，共 4 个页签：

| 页签 | 能做什么 |
|------|----------|
| **成员** | ➕ 添加家人账号（用户名/显示名/密码）、改密码、临时停用（停用后立即被踢下线）、彻底删除（连带清掉打印记录） |
| **记录** | 查看全家所有人近 200 条打印记录（谁、什么文件、什么时候、成功没有）+ 管理操作日志（谁改了什么，保留 90 天） |
| **设备** | 查看家里打印机在线状态、累计任务数；家庭端卡住时可一键强制断开（30 秒内自动重连） |
| **设置** | 每人每日打印上限（默认 30 份，防误刷纸）、登录记住天数、端到端加密开关，改完立即生效、重启不丢 |

**推荐的家庭账号规划**：管理员 1 个（自己用），每位家人 1 个普通账号。好处：某人手机丢了，进后台「停用」一下即可，别人不受影响。

> ⚠️ 首次登录默认账号 `admin / admin123`，**登录后第一件事：进「成员」→ 给 admin 改密码**。

## 十、交给家人

给家人发一条微信，内容照抄替换即可：

> 咱家打印网址：https://print.example.com
> 账号：xxx 密码：xxx（登录一次以后不用再输）
> 用法：点「选择文件」→ 点「开始打印」→ 回家取

帮老人做最后一步：手机浏览器打开网址 → 菜单 → **添加到主屏幕**。以后桌面图标一点即开，零辅导。

🎉 **到这里部署全部完成！** 下面是进阶内容和日常维护，按需阅读。

## 十一、进阶：端到端加密（推荐开启）

打印文件在**浏览器内加密**后才上传，云端只见密文、**服务器管理员也读不到内容**，家庭端收到后用同一密钥解密再打印。

开启只需两处填**同一个家庭密钥**（随便一段话，如 `我们家的小秘密2026`）：

1. **家庭端**：Docker 方式填 compose 文件里的 `PRINT_KEY=`；二进制方式启动命令末尾加 `-key 你的家庭密钥`
2. **家人浏览器**：首次打印时弹窗输入同一密钥（记住一次即可）

> 密钥不一致 → 任务提示「家庭密钥不匹配」直接失败，不会打出乱码。不填则走普通加密传输。

### 其他内置安全机制（无需配置）

| 层面 | 机制 |
|------|------|
| 传输 | 网页和任务全程 HTTPS/WSS 加密 |
| 账号 | 密码加密存储；输错 5 次锁 10 分钟；改密码/停用后所有旧登录立即失效 |
| 文件 | 打印完成立即删除（云端+家庭端双侧）；支持 PDF/JPG/PNG/DOCX/TXT，单个 ≤50MB，每人每天 30 个任务 |

## 十二、日志怎么看

### 在哪看

| 部署方式 | 云端日志 | 家庭端日志 |
|----------|---------|-----------|
| Docker / Compose | `docker logs -f fp-cloud` | `docker logs -f fp-agent` |
| 二进制 + systemd | `journalctl -u fp-cloud -f` | `journalctl -u fp-agent -f` |
| 只看错误 | 上述命令后加 ` \| grep ERR` | 同左 |

### 级别含义

| 前缀 | 含义 | 什么时候关注 |
|------|------|-------------|
| `[INF]` | 正常运转（登录、任务下发、打印完成） | 日常浏览 |
| `[WRN]` | 值得注意（登录失败、打印机离线、任务卡住） | 偶尔出现正常，频繁出现要查 |
| `[ERR]` | 出错了（打印失败、连不上、存储故障） | **第一时间看** |

### 能看到什么

**云端**：
- 启动横幅：版本、监听地址、每日限额、账号/任务数——一眼确认配置没改错
- 每条请求一行：`[INF] [请求ID] POST /api/login 200 53ms ip=1.2.x.x`（来源 IP 已脱敏；同请求 ID 的日志串成一条线）
- 登录成功/失败（含第几次、是否锁定）、任务提交/下发/完成/失败、管理员每步操作
- 管理操作另存数据库，管理后台「记录」页可翻看（保留 90 天）

**家庭端**：
- 启动横幅：版本、云端地址、打印机地址、是否加密——排查「连不上」先看这里
- 任务全流程：收到 → 排第几位 → 开始打印（份数/彩黑白/双单面）→ 完成/失败（含耗时）
- 打印机离线会写明原因（连不上 / 打印机返回错误）

### 详细程度与磁盘占用

- 云端：环境变量 `LOG_LEVEL=debug`（最详细）/ `info`（默认）/ `error`（只看错误）
- 家庭端：`-debug 1` 参数（或 compose 里 `DEBUG=1`）看探活等细节
- Docker 日志轮转已在 compose 模板里内置（10MB × 3 个文件）；二进制方式走 journald，系统自带轮转

## 十三、日常维护

### 备份（建议设置每天自动）

```bash
cd /opt/familyprint
bash backup.sh                          # 手动备份，生成 <1MB 的压缩包
bash restore.sh fp-backup-xxx.tar.gz    # 恢复
```

每天凌晨 3 点自动备份：

```bash
(crontab -l; echo "0 3 * * * cd /opt/familyprint && ./backup.sh >> backup.log 2>&1") | crontab -
```

### 换 VPS 迁移（家人完全无感）

1. 旧 VPS：`bash backup.sh`，把备份文件下载到电脑
2. 新 VPS：装 Docker → 创建同样的 `docker-compose.cloud.yml` → `bash restore.sh 备份文件`
3. 域名管理后台：把 A 记录的 IP 改成新 VPS

**家人手机和家里软路由什么都不用改**（都走域名）。

### 升级版本

```bash
# Docker 方式
cd /opt/familyprint
docker compose -f docker-compose.cloud.yml pull && docker compose -f docker-compose.cloud.yml up -d
docker compose -f docker-compose.agent.yml pull && docker compose -f docker-compose.agent.yml up -d

# 二进制一键方式（重跑一遍下载命令即可覆盖安装，然后重启服务）
curl -fsSL https://ghfast.top/https://raw.githubusercontent.com/mohan9898/familyprint/main/get.sh | sh
systemctl restart fp-cloud fp-agent   # 按你的实际服务名

# 二进制手动方式：重新 wget 覆盖 + systemctl restart
```

> 📦 历史版本与更新内容见 [Releases 页](https://github.com/mohan9898/familyprint/releases)。

## 十四、故障排查

| 症状 | 最可能的原因 → 解决办法 |
|------|------------------------|
| 网页打不开 | DNS 未生效（等几分钟，`ping 域名` 核对 IP）；反代/证书没配好（检查反代配置和 80/443 端口） |
| agent 连不上云端 | 两端 TOKEN 不一致（最常见！云端日志有「设备 token 不匹配」，agent 日志会直接提示核对 DEVICE_TOKEN）；域名写错；VPS 防火墙没放行 443 |
| 任务一直「排队中」 | 家庭端离线（看 agent 日志是「连不上」还是「已断开」）；或打印机没开机（agent 日志有「打印机离线」及原因） |
| 「失败：打印机连接失败」 | IPP 地址写错（先在家庭端 `curl -m 3 http://打印机IP:631` 测试）；打印机 IP 变了（绑静态 IP） |
| 双面/彩色不生效 | 个别品牌 IPP 属性名不同，提 [issue](https://github.com/mohan9898/familyprint/issues) 附品牌型号 |
| 「不支持的文件类型」 | 支持 PDF/JPG/PNG/DOCX/TXT；老 .doc 先转成 .docx 或 PDF |
| 过段时间又要登录 | 正常（默认 30 天），重登一次即可 |
| 下载二进制很慢/失败 | 加速前缀换成 `https://gh-proxy.com/`；或用一键脚本的 `MIRROR=` 参数 |
| 打印失败不知道哪一步出的问题 | 云端日志按「请求 ID」串起一次操作的全部日志；家庭端日志有「收到→排队→开始→完成/失败」每一步和耗时 |

---

## 附录

### 附录 A · fp-cloud 完整参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-secret` | 会话密钥（必改） | 环境变量 `SECRET_KEY` |
| `-token` | 设备 token（必改，与家庭端一致） | 环境变量 `DEVICE_TOKEN` |
| `-data` | 数据目录 | `data` |
| `-listen` | 监听地址（对外服务用 `:8000`） | `:8000` |
| `-session-days` | 会话有效天数 | `30` |
| `-daily-limit` | 每用户每日任务上限（管理员网页设置可覆盖） | `30` |
| `-job-timeout` | 任务超时分钟数：家庭端离线超过该时长仍未回报，任务判失败 | `5` |

### 附录 B · 全平台下载对照表

[Releases 页](https://github.com/mohan9898/familyprint/releases) 按设备选择：

| 设备 | Agent 文件 | Cloud 文件 |
|------|-----------|-----------|
| x86 软路由 / NAS / VPS / 旧电脑 | `fp-agent-linux-amd64` | `fp-cloud-linux-amd64` |
| 树莓派 4/5、ARM 盒子（N1/R2S/R4S） | `fp-agent-linux-arm64` | `fp-cloud-linux-arm64` |
| 树莓派 3、32 位 ARM | `fp-agent-linux-arm` | `fp-cloud-linux-arm` |
| Mac（Intel / Apple Silicon） | `fp-agent-darwin-*` | `fp-cloud-darwin-*` |
| Windows | `fp-agent-windows-amd64.exe` | `fp-cloud-windows-amd64.exe` |

> 💡 `arm64` 就是 `aarch64`（同一种架构的两种叫法）。不确定设备架构时，在设备上执行 `uname -m`：输出 `aarch64` → 下载 `*-linux-arm64`；输出 `x86_64` → 下载 `*-linux-amd64`；输出 `armv7l` → 下载 `*-linux-arm`。
>
> USB 打印机暂不支持（需 CUPS 桥接），当前面向 IPP 网络打印机。

### 附录 C · fp-agent 完整参数

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-cloud` | 云端地址，多个用逗号分隔（换 VPS 平滑切换） | 环境变量 `CLOUD_URLS` |
| `-token` | 设备 token（与云端一致） | 环境变量 `DEVICE_TOKEN` |
| `-printer` | 打印机 IPP 地址 | `ipp://192.168.1.100:631/ipp/print` |
| `-name` | 打印机显示名 | `家里打印机` |
| `-key` | 端到端加密家庭密钥（可选） | 空=不加密 |
| `-mock` | 模拟打印调试 | `0` |
| `-debug` | 调试日志（1=输出探活等细节） | `0` |

> 💡 对应的环境变量：`CLOUD_URLS` / `DEVICE_TOKEN` / `PRINTER_IPP_URL` / `PRINTER_NAME` / `PRINT_KEY` / `MOCK` / `DEBUG`——Docker compose 里用的就是这些。

### 附录 D · 从源码构建

```bash
cd cloud-go && go build -o fp-cloud .   # 需 Go 1.22+
cd agent-go && go build -o fp-agent .

bash build-all.sh   # 全平台交叉编译，产出 release/ 目录
```

推送标签 `v*` 后 GitHub Actions 自动编译全部平台并发布：

```bash
git tag v1.0.2 && git push origin v1.0.2
```

### 附录 E · 项目结构

```
familyprint/
├── cloud-go/                 # 云端服务端（含内嵌网页前端）
├── agent-go/                 # 家庭端客户端
├── get.sh                    # 二进制一键下载脚本（国内加速）
├── docker-compose.cloud.yml  # 云端编排
├── docker-compose.agent.yml  # 家庭端编排
├── build-all.sh              # 全平台交叉编译
├── backup.sh / restore.sh    # 数据备份与恢复
└── .github/workflows/        # CI + 自动发布
```

---

## 🤖 纯 AI 编写声明

本项目的**全部内容——代码、架构设计、网页前端、部署配置、CI/CD 工作流与本 README——均由 AI（CodeBuddy）独立编写**，人类仅提出需求、验收成果并决定发布。项目按「需求对话 → 方案设计 → 编码实现 → 模拟运行测试 → 开源发布」的流程由 AI 全程交付，欢迎参考这一 AI 全流程开发的实践案例。

## 📄 许可证

本项目以 [PolyForm Noncommercial 1.0.0](./LICENSE) 协议开源——**允许个人、家庭、学习等非商业用途自由使用/修改/分发，禁止商用**。商业授权请联系作者。
