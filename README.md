<div align="center">

# FamilyPrint「家印」

**全家共用的远程打印服务 · 网页即开即用 · 老人零学习成本**

[![License](https://img.shields.io/badge/License-PolyForm_NC_1.0.0-orange)](./LICENSE)
[![Release](https://img.shields.io/github/v/release/mohan9898/familyprint)](https://github.com/mohan9898/familyprint/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/mohan9898/familyprint/ci.yml?branch=main&label=CI)](https://github.com/mohan9898/familyprint/actions)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Built by AI](https://img.shields.io/badge/Built_by-100%25_AI-8A2BE2)](#-纯-ai-编写声明)

家人在微信/浏览器里打开网址 → 选文件 → 点打印 → 回家取件。
**零端口映射、零公网 IP、零 DDNS**，云端全密文中转，单二进制部署。

</div>

---

## ✨ 特性

| | |
|---|---|
| 🧓 **老人友好** | 大按钮大字号全中文，登录一次 30 天免密，微信内直开 |
| 🔒 **端到端加密** | 浏览器内 AES-256-GCM 加密，云端全程不可读文件内容 |
| 🛡️ **零暴露面** | 家庭端纯出站 WSS 连接，路由器不开任何入站端口 |
| 📦 **极轻量** | 全栈 Go 单二进制：云端 ~15MB、家庭端 ~8MB 内存 |
| 🖨️ **IPP 直连** | 无需 CUPS，Agent 直连网络打印机 |
| 💾 **易迁移易备份** | 数据 <1MB，backup/restore 脚本一键完成，换 VPS 不动家庭端 |
| ⚙️ **全平台** | linux(amd64/arm64/armv7) / macOS / Windows，Docker 与二进制双支持 |

## 🏗️ 架构

```
家人浏览器（微信内也可直开）
   │  HTTPS（自动证书）
   ▼
云端 VPS：fp-cloud（Go 单二进制 + Caddy 自动 HTTPS）
   │  账号 · 任务队列 · 文件中转（可 E2EE 密文）
   ▲  WSS 出站长连接（家庭端主动连云端）
家庭软路由 / NAS / 树莓派：fp-agent（Go 单二进制）
   │  IPP 直连
   ▼
打印机
```

> 对标 cups-yun-agent 的「本地 Agent + 云端中继」架构，入口统一为网页/H5。家庭端主动出站连接云端，因此**换 VPS 只需改域名 DNS，家人端和软路由完全不用动**。

## 📋 部署前准备

| 需要 | 说明 |
|------|------|
| 备案域名 | 如 `print.example.com` |
| 公网 VPS | 1核512MB 即可 |
| 家庭常开设备 | 软路由 / NAS / 树莓派 / 旧电脑，能联网 |
| 网络打印机 | 已接入家庭局域网并固定 IP |

先生成两个随机值（后面反复用到）：

```bash
openssl rand -hex 32   # DEVICE_TOKEN 设备令牌（云端与家庭端一致）
openssl rand -hex 32   # SECRET_KEY 会话密钥（仅云端）
```

---

## 🚀 部署教程（约 30 分钟，从零到出纸）

### 第 1 步 · 域名解析

1. 域名控制台添加 A 记录：`print` → VPS 公网 IP
2. 该记录 **TTL 调为 300 秒**（日后换 VPS 生效快）

### 第 2 步 · 云端部署（VPS）

#### 2.1 安装 Docker 与 Docker Compose（已装跳过）

现在的 Docker 自带 Compose 插件（命令为 `docker compose`，无横线），一条脚本装齐：

```bash
curl -fsSL https://get.docker.com | sh     # 一键装 Docker Engine + Compose 插件
systemctl enable --now docker

# 验证：两条都出版本号即装好
docker --version
docker compose version
```

> 老系统若只有 `docker-compose`（带横线的独立 v1 版）也能用，把后文命令里的 `docker compose` 换成 `docker-compose` 即可。

#### 2.2 上传项目并启动

```bash
scp -r familyprint/ root@VPS_IP:/opt/familyprint
ssh root@VPS_IP && cd /opt/familyprint
```

编辑两个文件后启动：

- `docker-compose.cloud.yml`：填入 `SECRET_KEY`、`DEVICE_TOKEN`
- `Caddyfile`：改为你的真实域名

```bash
docker compose -f docker-compose.cloud.yml up -d

# 验证
docker logs fp-cloud   # 应看到 "FamilyPrint-Cloud (Go) 监听 :8000"
docker logs fp-caddy   # 应看到证书签发成功（1-2 分钟）
```

打开 `https://print.example.com`，用默认管理员 `admin / admin123` 登录，**立即改密码**。

<details>
<summary>📦 免编译方式：直接用 GHCR 镜像（推荐，VPS 上不装 Go 也能跑）</summary>

编辑 `docker-compose.cloud.yml`：**注释掉 `build: ./cloud-go`，取消注释 `image:` 行**：

```yaml
  cloud:
    image: ghcr.io/mohan9898/familyprint-cloud:latest   # ← 启用这行
    # build: ./cloud-go                                  # ← 注释这行
```

之后 `docker compose up -d` 自动拉取对应架构镜像（amd64/arm64/armv7）。此方式 VPS 上**无需 Go 环境，甚至无需上传源码**——只需 `docker-compose.cloud.yml` 和 `Caddyfile` 两个文件。

</details>

<details>
<summary>📦 二进制方式运行云端（不用 Docker）</summary>

```bash
# 1. 下载二进制（按 CPU 架构选择，一般 VPS 是 amd64；其他：arm64 / arm / darwin-* / windows）
wget https://github.com/mohan9898/familyprint/releases/latest/download/fp-cloud-linux-amd64 -O /usr/local/bin/fp-cloud
chmod +x /usr/local/bin/fp-cloud

# 2. systemd 常驻
cat > /etc/systemd/system/fp-cloud.service <<'EOF'
[Unit]
Description=FamilyPrint Cloud
After=network-online.target
[Service]
Environment=SECRET_KEY=改成随机长字符串
Environment=DEVICE_TOKEN=改成设备token
Environment=DATA_DIR=/var/lib/familyprint
Environment=LISTEN=:8000
ExecStart=/usr/local/bin/fp-cloud
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-cloud
journalctl -u fp-cloud -f   # 应看到 "监听 :8000"
```

HTTPS 交给 Caddy：`apt install caddy`，在 `Caddyfile` 写 `print.example.com { reverse_proxy 127.0.0.1:8000 }` 即自动签证书。

</details>

### 第 3 步 · 打印机 IPP 地址

1. 打印机面板查看 IP（如 `192.168.1.100`），**在路由器绑定静态 IP**
2. IPP 地址通常为 `ipp://192.168.1.100:631/ipp/print`（HP 多为 `/ipp/print`，Epson/Canon/Brother 见说明书）
3. 验证连通性：

```bash
curl -s -m 3 http://192.168.1.100:631 >/dev/null && echo "打印机 631 端口可达"
```

### 第 4 步 · 家庭端 Agent（二选一）

#### 方式 A · Docker Compose（推荐）

软路由/NAS 需要先有 Docker（OpenWrt 一般在「系统→软件包」搜 `docker` 或 `dockerd` 安装；Debian/Ubuntu 用同一条 `get.docker.com` 脚本）：

```bash
DEVICE_TOKEN=与云端一致 \
PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print \
docker compose -f docker-compose.agent.yml up -d

docker logs -f fp-agent   # 应看到 "已连接云端 wss://print.example.com/ws/agent"
```

免编译用 GHCR 镜像：编辑 `docker-compose.agent.yml`，**注释掉 `build: ./agent-go`，取消注释 `image:` 行**（此方式软路由上无需 Go 环境，只需这一个 compose 文件）：

```yaml
  agent:
    image: ghcr.io/mohan9898/familyprint-agent:latest   # ← 启用这行
    # build: ./agent-go                                  # ← 注释这行
```

变量也可以写在 compose 文件同目录的 `.env` 文件里（推荐，重启不丢）：

```bash
cat > .env <<'EOF'
DEVICE_TOKEN=与云端一致
PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print
# PRINT_KEY=你的家庭密钥    ← 开启端到端加密（可选）
EOF
docker compose -f docker-compose.agent.yml up -d
```

#### 方式 B · 二进制（不用 Docker，内存最省）

直接从 [Releases 页](https://github.com/mohan9898/familyprint/releases) 下载对应平台文件，无需 Go 环境：

| 设备 | 下载文件 |
|------|----------|
| x86 软路由 / NAS / 旧电脑 | `fp-agent-linux-amd64` |
| 树莓派 4/5、ARM 盒子（N1/R2S/R4S） | `fp-agent-linux-arm64` |
| 树莓派 3、32 位 ARM | `fp-agent-linux-arm` |
| Mac | `fp-agent-darwin-amd64` / `fp-agent-darwin-arm64` |
| Windows | `fp-agent-windows-amd64.exe` |

```bash
# 示例：ARM64 软路由
wget https://github.com/mohan9898/familyprint/releases/latest/download/fp-agent-linux-arm64 -O /usr/local/bin/fp-agent
chmod +x /usr/local/bin/fp-agent
```

**systemd 常驻**（标准 Linux / NAS）：

```ini
# /etc/systemd/system/fp-agent.service
[Unit]
Description=FamilyPrint Agent
After=network-online.target
[Service]
Environment=CLOUD_URLS=wss://print.example.com/ws/agent
Environment=DEVICE_TOKEN=与云端一致
Environment=PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print
# PRINT_KEY=你的家庭密钥   ← 开启端到端加密（见下文）
ExecStart=/usr/local/bin/fp-agent
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload && systemctl enable --now fp-agent
journalctl -u fp-agent -f   # 应看到 "已连接云端"
```

**OpenWrt（procd）**：

```bash
cat > /etc/init.d/fp-agent <<'EOF'
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
    procd_open_instance
    procd_set_param command /usr/local/bin/fp-agent
    procd_set_param env CLOUD_URLS=wss://print.example.com/ws/agent
    procd_set_param env DEVICE_TOKEN=与云端一致
    procd_set_param env PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print
    procd_set_param respawn
    procd_close_instance
}
EOF
chmod +x /etc/init.d/fp-agent && /etc/init.d/fp-agent enable && /etc/init.d/fp-agent start
```

### 第 5 步 · 全链路验证

- [ ] 手机**关 WiFi 用流量**打开网址 → 登录 → 传 PDF → 点"开始打印" → **出纸**
- [ ] 份数=2、双面 → 出纸正确
- [ ] 微信内打开网址 → 出现提示卡 → 能正常上传
- [ ] 软路由重启 → agent 自动重连 → 打印机恢复在线
- [ ] 老人手机"添加到主屏幕"→ 以后一点即开

### 第 6 步 · 交给家人（一次性）

> 咱家打印网址：https://print.example.com
> 账号 xxx 密码 xxx（登录一次以后就不用输了）
> 用法：点"选择文件" → 点"开始打印" → 回家取

---

## 📄 docker-compose 完整配置（可直接复制使用）

以下两个文件即为仓库内 `docker-compose.cloud.yml` / `docker-compose.agent.yml` 的可直接使用版本——镜像走 GHCR 公共仓库，**无需源码、无需编译**，改好 3 个值即可启动。

**云端 · docker-compose.cloud.yml**（VPS 上 `mkdir familyprint && cd familyprint && nano docker-compose.cloud.yml` 粘贴保存）：

```yaml
# 云端（VPS）—— FamilyPrint-Cloud + Caddy 自动 HTTPS
services:
  cloud:
    image: ghcr.io/mohan9898/familyprint-cloud:latest   # 多架构：amd64/arm64/armv7 自动匹配
    container_name: fp-cloud
    restart: unless-stopped
    environment:
      - SECRET_KEY=改成一段随机长字符串          # openssl rand -hex 32
      - DEVICE_TOKEN=改成设备token_与家庭端一致   # openssl rand -hex 32
      - DATA_DIR=/app/data
      - LISTEN=:8000
    volumes:
      - cloud-data:/app/data
    ports:
      - "127.0.0.1:8000:8000"   # 仅本机，由 Caddy 反代对外

  caddy:
    image: caddy:latest
    container_name: fp-caddy
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data

volumes:
  cloud-data:
  caddy-data:
```

**云端 · Caddyfile**（同目录保存，改域名）：

```
print.example.com {
    reverse_proxy cloud:8000
}
```

启动：`docker compose -f docker-compose.cloud.yml up -d`

**家庭端 · docker-compose.agent.yml**（软路由上保存，或直接用命令行变量）：

```yaml
# 家庭端（软路由/NAS）—— Go Agent，IPP 直连打印机，无 CUPS
# 内存占用 ~10MB；host 网络模式以直接访问家庭网段内的打印机
services:
  agent:
    image: ghcr.io/mohan9898/familyprint-agent:latest   # 多架构：amd64/arm64/armv7 自动匹配
    container_name: fp-agent
    restart: unless-stopped
    network_mode: host
    environment:
      - CLOUD_URLS=wss://print.example.com/ws/agent              # ← 改成你的域名
      - DEVICE_TOKEN=改成设备token_与云端一致                      # ← 改成与云端一致
      - PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print        # ← 改成打印机 IPP 地址
      - PRINTER_NAME=家里打印机
      - MOCK=0
      - PRINT_KEY=                        # 端到端加密家庭密钥（可选）：填上后浏览器首次打印时输入同一密钥
```

启动（变量也可以写进同目录 `.env` 文件后直接 `docker compose up -d`）：

```bash
DEVICE_TOKEN=与云端一致 \
PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print \
docker compose -f docker-compose.agent.yml up -d
```

> 本地构建镜像（不用 GHCR）：把 `image:` 行换回 `build: ./cloud-go` / `build: ./agent-go` 即可。

---

## 🔐 安全说明

### 端到端加密（推荐开启）

打印文件在**浏览器内加密**（AES-256-GCM，密钥由家庭密钥经 PBKDF2 派生），经云端中转全程密文，**云端不可读**；家庭端 Agent 用同一密钥解密后再送打印机。

开启只需两步：

1. 家庭端 Agent 配置环境变量 `PRINT_KEY=你的家庭密钥`
2. 家人浏览器首次打印时弹窗输入同一密钥（存 localStorage，一次即可）

> 密钥不匹配 → 任务失败并提示"家庭密钥不匹配"，不会打出乱码。不设置 `PRINT_KEY` 则走明文路径。加密文件在记录中显示为 `print.enc`（文件名也在密文内）。

### 其他内置安全机制

| 层面 | 机制 |
|------|------|
| 传输 | 全程 HTTPS / WSS；Cookie 为 HttpOnly SameSite |
| 设备接入 | token 走 `X-Device-Token` 头不进 URL；常数时间比较防时序攻击；单设备互斥（新连接顶替旧连接） |
| 账号 | 口令 pbkdf2 哈希存储；登录失败 5 次锁定 10 分钟 |
| 数据 | 上传白名单（PDF/JPG/PNG/DOCX/TXT）≤50MB、每日 30 任务；打印完成即删（云端+家庭端双侧），失败任务 24h 兜底清理 |

**上线前检查单**：域名走 HTTPS ✅ · 两端 DEVICE_TOKEN 为 64 位随机串且一致 ✅ · SECRET_KEY 与之不同 ✅ · VPS 防火墙仅放行 80/443 ✅

---

## 🔄 备份与迁移

### 日常备份

```bash
bash backup.sh                       # 生成 fp-backup-xxx.tar.gz（<1MB，含账号与记录）
bash restore.sh fp-backup-xxx.tar.gz # 恢复
```

自动识别 Docker 卷（`fp_cloud-data`）与本地目录（`cloud-go/data`）。建议加 crontab：

```
0 3 * * * cd /opt/familyprint && ./backup.sh >> backup.log 2>&1
```

### 换 VPS 迁移（约 5 分钟 + DNS 生效）

1. 旧 VPS：`bash backup.sh` → 下载备份文件
2. 新 VPS：上传项目与备份 → `bash restore.sh fp-backup-xxx.tar.gz`
3. 域名 A 记录改指新 VPS IP —— **家人端和软路由完全不用动**

> 兜底：软路由 `CLOUD_URLS` 支持逗号分隔多地址（如 `wss://新域名/ws/agent,wss://旧直连/ws/agent`），DNS 切换间隙自动轮询，打印不中断。

---

## 🔧 故障排查

| 症状 | 原因与处理 |
|------|-----------|
| 网页打不开 | DNS 未生效（`ping 域名` 核对 IP）；Caddy 证书未签下（`docker logs fp-caddy`，确认 80/443 放行且域名已备案） |
| agent 连不上云端 | 两端 token 不一致；域名写错；VPS 防火墙未放行 443 |
| 任务一直"排队中" | agent 离线（`docker logs fp-agent`）；打印机离线 |
| "失败：打印机连接失败" | PRINTER_IPP_URL 写错（先 curl 验证 631 端口）；打印机 IP 变了（绑定静态 IP） |
| 双面/彩色不生效 | 个别品牌 IPP 属性名有差异，提 issue 附品牌型号 |
| "不支持的文件类型" | 支持 PDF/JPG/PNG/DOCX/TXT；.doc 先另存为 .docx 或 PDF |
| 30 天后又要求登录 | 正常，重登一次即可；可改 `SESSION_DAYS` |

## 🧰 日常运维速查

| 操作 | 命令 |
|------|------|
| 云端日志 | `docker logs -f fp-cloud`（二进制：`journalctl -u fp-cloud -f`） |
| Agent 日志 | `docker logs -f fp-agent`（二进制：`journalctl -u fp-agent -f`） |
| 备份数据 | `./backup.sh` |
| 升级版本 | 更新镜像/二进制 → `docker compose ... up -d` 或 `systemctl restart fp-agent` |
| 加家人账号 | 目前经 sqlite 添加（见第 2 步），网页端账号管理在 V1.1 |

---

## 🖥️ 平台支持

Go 交叉编译，`./build-all.sh` 一键产出全部平台二进制（Release 页可直接下载）：

| 平台 | Agent | Cloud | 典型设备 |
|------|:---:|:---:|----------|
| linux/amd64 | ✅ | ✅ | x86 软路由、NAS、VPS、旧电脑 |
| linux/arm64 | ✅ | ✅ | 树莓派 4/5、ARM 盒子、N1/R2S/R4S |
| linux/arm v7 | ✅ | ✅ | 树莓派 3、老款 32 位 ARM 设备 |
| darwin（Intel/Apple Silicon） | ✅ | ✅ | Mac |
| windows/amd64 | ✅ | ✅ | Windows 电脑 / 闲置笔记本 |

> USB 打印机暂不支持（需 CUPS 桥接），当前面向 IPP 网络打印机。

## 📁 项目结构

```
familyprint/
├── cloud-go/                 # 云端：账号 + 任务队列 + 文件中转（含内嵌网页前端）
│   ├── main.go
│   ├── static/index.html     # 老人友好单页前端
│   └── Dockerfile
├── agent-go/                 # 家庭端：WSS 连云端 + IPP 直连打印机
│   ├── main.go
│   └── Dockerfile
├── docker-compose.cloud.yml  # 云端编排（fp-cloud + Caddy）
├── docker-compose.agent.yml  # 家庭端编排
├── Caddyfile                 # HTTPS 反代配置
├── build-all.sh              # 全平台交叉编译
├── backup.sh / restore.sh    # 数据备份与恢复
└── .github/workflows/        # CI + Release 自动构建发布
```

## 🔄 从源码构建 & 发布新版本

```bash
# 本地编译（需 Go 1.22+）
cd cloud-go && go build -o fp-cloud .
cd agent-go && go build -o fp-agent .

# 全平台交叉编译
bash build-all.sh   # 产出 release/ 目录
```

推送标签 `v*` 后，GitHub Actions 自动编译 12 个平台二进制挂到 Release，并推送多架构 Docker 镜像到 GHCR：

```bash
git tag v1.0.1 && git push origin v1.0.1
```

## 🤖 纯 AI 编写声明

本项目的**全部内容——代码、架构设计、网页前端、部署配置、CI/CD 工作流与本 README——均由 AI（CodeBuddy）独立编写**，人类仅提出需求、验收成果并决定发布。项目按「需求对话 → 方案设计 → 编码实现 → 模拟运行测试 → 开源发布」的流程由 AI 全程交付，欢迎参考这一 AI 全流程开发的实践案例。

## 📄 许可证

本项目以 [PolyForm Noncommercial 1.0.0](./LICENSE) 协议开源——**允许个人、家庭、学习等非商业用途自由使用/修改/分发，禁止商用**。商业授权请联系作者。
