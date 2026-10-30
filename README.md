<div align="center">

# FamilyPrint「家印」

**全家共用的远程打印服务 · 网页即开即用 · 老人零学习成本**

[![License](https://img.shields.io/badge/License-PolyForm_NC_1.0.0-orange)](./LICENSE)
[![Release](https://img.shields.io/github/v/release/mohan9898/familyprint)](https://github.com/mohan9898/familyprint/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/mohan9898/familyprint/ci.yml?branch=main&label=CI)](https://github.com/mohan9898/familyprint/actions)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)

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

```bash
# 安装 Docker（已装跳过）
curl -fsSL https://get.docker.com | sh && systemctl enable --now docker

# 上传项目并进入
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
<summary>📦 免编译方式：直接用 GHCR 镜像（推荐）</summary>

```yaml
# docker-compose.cloud.yml 中取消注释并改为你的用户名
image: ghcr.io/mohan9898/familyprint-cloud:latest
```
镜像多架构（amd64/arm64/armv7），会自动匹配。

</details>

<details>
<summary>📦 二进制方式运行云端</summary>

```bash
# 从 Release 下载对应平台二进制，或本地编译
cd cloud-go && go build -o fp-cloud .
./fp-cloud   # 需先设置环境变量，并用 nginx/caddy 反代 HTTPS
```

</details>

### 第 3 步 · 打印机 IPP 地址

1. 打印机面板查看 IP（如 `192.168.1.100`），**在路由器绑定静态 IP**
2. IPP 地址通常为 `ipp://192.168.1.100:631/ipp/print`（HP 多为 `/ipp/print`，Epson/Canon/Brother 见说明书）
3. 验证连通性：

```bash
curl -s -m 3 http://192.168.1.100:631 >/dev/null && echo "打印机 631 端口可达"
```

### 第 4 步 · 家庭端 Agent（二选一）

#### 方式 A · Docker（推荐）

```bash
DEVICE_TOKEN=与云端一致 \
PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print \
docker compose -f docker-compose.agent.yml up -d

docker logs -f fp-agent   # 应看到 "已连接云端 wss://print.example.com/ws/agent"
```

GHCR 镜像同理：`image: ghcr.io/mohan9898/familyprint-agent:latest`

#### 方式 B · 裸二进制（内存极度紧张时）

```bash
# 任意有 Go 的机器编译（或直接用 Release 现成二进制）
cd agent-go && GOOS=linux GOARCH=arm64 go build -o fp-agent .
scp fp-agent root@软路由IP:/usr/local/bin/ && chmod +x /usr/local/bin/fp-agent
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

## 📄 许可证

本项目以 [PolyForm Noncommercial 1.0.0](./LICENSE) 协议开源——**允许个人、家庭、学习等非商业用途自由使用/修改/分发，禁止商用**。商业授权请联系作者。
