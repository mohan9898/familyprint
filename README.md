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

> 📌 **新手请全程使用「Docker Compose」方式**（本教程主线），不用装 Go、不用下载代码，复制粘贴即可。二进制等高级方式见文末附录。

---

## 二、开始前的准备（10 分钟）

### ✅ 你需要有的东西

| 序号 | 需要 | 检查方法 | 没有怎么办 |
|:---:|------|----------|-----------|
| ① | 一个**备案域名**（如 `print.example.com`） | 能登录域名管理后台 | 去阿里云/腾讯云买一个并备案 |
| ② | 一台**公网 VPS**（1核512M 足够） | SSH 能连上（教程会教） | 任意云厂商轻量服务器 |
| ③ | 一台**家里常开的设备**（软路由/NAS/树莓派/旧电脑） | 能开机、能上网 | — |
| ④ | 一台**网络打印机**（WiFi/网线接入家里局域网） | 打印机屏幕里能查到 IP（如 `192.168.1.100`） | USB 打印机暂不支持 |

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

---

## 三、云端部署（VPS 上，约 15 分钟）

### 第 1 步：装 Docker（已装跳过）

SSH 登录 VPS（Windows 可用 PowerShell 的 `ssh root@你的VPS_IP`），执行：

```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker

# 验证：两条命令都能显示版本号 = 装好了
docker --version
docker compose version
```

### 第 2 步：创建两个文件

```bash
mkdir -p /opt/familyprint && cd /opt/familyprint
```

**文件 1**：执行下面整段命令，自动创建 `docker-compose.cloud.yml`（创建后把 2 个「必改」值改掉）：

```bash
cat > docker-compose.cloud.yml <<'EOF'
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
    volumes:
      - cloud-data:/app/data
    ports:
      - "8000:8000"

volumes:
  cloud-data:
EOF
```

改文件用编辑器：`nano docker-compose.cloud.yml`（改完按 `Ctrl+O` 保存、`Ctrl+X` 退出）。

**文件 2**：在你自己的 HTTPS 反代里，把域名 `print.example.com` 回源到 `VPS_IP:8000`（宝塔/Nginx/1Panel/云负载均衡均可，按你平时的习惯配置即可）。

### 第 3 步：启动并验证

```bash
docker compose -f docker-compose.cloud.yml up -d

# 验证：看到 "监听 :8000" 就是成功
docker logs fp-cloud
```

**✅ 检查点 1**：浏览器打开 `https://print.example.com` 能看到登录页 → 登录默认账号 `admin / admin123` → **立即改密码**！

---

## 四、家庭端部署（家里设备上，约 10 分钟）

### 第 1 步：找到打印机的 IPP 地址

1. 打印机屏幕：设置 → 网络设置 → 查看 IP（如 `192.168.1.100`）
2. 到路由器管理页，给这个 IP 绑定**静态分配**（防止 IP 变动）
3. IPP 地址 = `ipp://192.168.1.100:631/ipp/print`（把 IP 换成你的；个别品牌路径不同，试不通看[故障排查](#九故障排查)）

### 第 2 步：安装并启动 fp-agent（二选一）

#### 方式 A：Docker（软路由/NAS 有 Docker 就用这个）

家里设备上执行，创建 `docker-compose.agent.yml`：

```bash
cat > docker-compose.agent.yml <<'EOF'
services:
  agent:
    image: ghcr.io/mohan9898/familyprint-agent:latest
    container_name: fp-agent
    restart: unless-stopped
    network_mode: host
    environment:
      - CLOUD_URLS=wss://print.example.com/ws/agent      # ← 必改：你的域名
      - DEVICE_TOKEN=把这里改成第1串随机字符              # ← 必改（和云端一致）
      - PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print # ← 必改：打印机地址
      - PRINTER_NAME=家里打印机
      - MOCK=0
      - PRINT_KEY=                                        # 可选：端到端加密密钥
EOF

docker compose -f docker-compose.agent.yml up -d

# 验证：看到 "已连接云端" 就是成功
docker logs fp-agent
```

> OpenWrt 装 Docker：网页管理界面 → 系统 → 软件包 → 搜索 `dockerd` 安装。

#### 方式 B：二进制（不用 Docker，一条命令直接跑，和 EasyTier 用法一样）

```bash
# 1. 下载（按设备选文件，对照表在附录 C）
wget https://github.com/mohan9898/familyprint/releases/latest/download/fp-agent-linux-arm64 -O /usr/local/bin/fp-agent
chmod +x /usr/local/bin/fp-agent

# 2. 一条命令启动（改 3 个值）
fp-agent -cloud wss://print.example.com/ws/agent -token 第1串随机字符 -printer ipp://192.168.1.100:631/ipp/print
```

想开机自启：用 systemd 或 OpenWrt procd，完整配置见[附录 D](#附录-d家庭端开机自启配置)。

**✅ 检查点 2**：回到网页打印页，顶部打印机状态显示**在线**。

---

## 五、测试打印（5 分钟）

手机**关掉 WiFi 用流量**（模拟在外场景），完成下面清单：

- [ ] 打开 `https://print.example.com` → 登录
- [ ] 传一个 PDF → 点「开始打印」→ **家里打印机出纸**
- [ ] 改份数=2、开双面 → 出纸正确
- [ ] 微信里打开同一个网址 → 有提示卡 → 也能正常上传打印

## 六、交给家人（一次性设置，2 分钟）

给家人发一条微信，内容照抄替换即可：

> 咱家打印网址：https://print.example.com
> 账号：xxx 密码：xxx（登录一次以后不用再输）
> 用法：点「选择文件」→ 点「开始打印」→ 回家取

帮老人做最后一步：手机浏览器打开网址 → 菜单 → **添加到主屏幕**。以后桌面图标一点即开，零辅导。

🎉 **到这里部署全部完成！** 下面是进阶内容和日常维护，按需阅读。

---

## 七、进阶：端到端加密（推荐开启）

打印文件在**浏览器内加密**后才上传，云端只见密文、**服务器管理员也读不到内容**，家庭端收到后用同一密钥解密再打印。

开启只需两处填**同一个家庭密钥**（随便一段话，如 `我们家的小秘密2026`）：

1. **家庭端**：Docker 方式填 `docker-compose.agent.yml` 里的 `PRINT_KEY=`；二进制方式启动命令末尾加 `-key 你的家庭密钥`
2. **家人浏览器**：首次打印时弹窗输入同一密钥（记住一次即可）

> 密钥不一致 → 任务提示「家庭密钥不匹配」直接失败，不会打出乱码。不填则走普通加密传输。

### 其他内置安全机制（无需配置）

| 层面 | 机制 |
|------|------|
| 传输 | 网页和任务全程 HTTPS/WSS 加密 |
| 账号 | 密码加密存储；输错 5 次锁 10 分钟 |
| 文件 | 打印完成立即删除（云端+家庭端双侧）；支持 PDF/JPG/PNG/DOCX/TXT，单个 ≤50MB，每人每天 30 个任务 |

---

## 八、日常维护

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

### 常用命令速查

| 想做什么 | Docker 方式 | 二进制方式 |
|----------|------------|-----------|
| 看云端日志 | `docker logs -f fp-cloud` | `journalctl -u fp-cloud -f` |
| 看家庭端日志 | `docker logs -f fp-agent` | `journalctl -u fp-agent -f` |
| 重启云端 | `docker compose -f docker-compose.cloud.yml restart` | `systemctl restart fp-cloud` |
| 重启家庭端 | `docker compose -f docker-compose.agent.yml restart` | `systemctl restart fp-agent` |
| 升级版本 | `docker compose ... pull && docker compose ... up -d` | 重新下载二进制覆盖后重启 |

## 九、故障排查

| 症状 | 最可能的原因 → 解决办法 |
|------|------------------------|
| 网页打不开 | DNS 未生效（等几分钟，`ping 域名` 核对 IP）；反代/证书没配好（检查反代配置和 80/443 端口） |
| agent 连不上云端 | 两端 TOKEN 不一致（最常见！逐字核对）；域名写错；VPS 防火墙没放行 443 |
| 任务一直「排队中」 | 家庭端离线（看 agent 日志）；或打印机没开机 |
| 「失败：打印机连接失败」 | IPP 地址写错（先在家庭端 `curl -m 3 http://打印机IP:631` 测试）；打印机 IP 变了（绑静态 IP） |
| 双面/彩色不生效 | 个别品牌 IPP 属性名不同，提 [issue](https://github.com/mohan9898/familyprint/issues) 附品牌型号 |
| 「不支持的文件类型」 | 支持 PDF/JPG/PNG/DOCX/TXT；老 .doc 先转成 .docx 或 PDF |
| 过段时间又要登录 | 正常（默认 30 天），重登一次即可 |

---

## 附录

### 附录 A · 二进制运行云端（不用 Docker）

```bash
# 下载（amd64 为主流，其他架构见附录 C）
wget https://github.com/mohan9898/familyprint/releases/latest/download/fp-cloud-linux-amd64 -O /usr/local/bin/fp-cloud
chmod +x /usr/local/bin/fp-cloud

# 一条命令启动
fp-cloud -secret 你的会话密钥 -token 你的设备token -data /var/lib/familyprint -listen 127.0.0.1:8000
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-secret` | 会话密钥（必改） | 环境变量 `SECRET_KEY` |
| `-token` | 设备 token（必改，与家庭端一致） | 环境变量 `DEVICE_TOKEN` |
| `-data` | 数据目录 | `data` |
| `-listen` | 监听地址 | `:8000` |
| `-session-days` | 会话有效天数 | `30` |

### 附录 B · 云端开机自启（systemd）

```bash
cat > /etc/systemd/system/fp-cloud.service <<'EOF'
[Unit]
Description=FamilyPrint Cloud
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fp-cloud -secret 改成会话密钥 -token 改成设备token -data /var/lib/familyprint -listen 127.0.0.1:8000
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-cloud
journalctl -u fp-cloud -f   # 应看到 "监听 :8000"
```

### 附录 C · 全平台下载对照表

[Releases 页](https://github.com/mohan9898/familyprint/releases) 按设备选择：

| 设备 | Agent 文件 | Cloud 文件 |
|------|-----------|-----------|
| x86 软路由 / NAS / VPS / 旧电脑 | `fp-agent-linux-amd64` | `fp-cloud-linux-amd64` |
| 树莓派 4/5、ARM 盒子（N1/R2S/R4S） | `fp-agent-linux-arm64` | `fp-cloud-linux-arm64` |
| 树莓派 3、32 位 ARM | `fp-agent-linux-arm` | `fp-cloud-linux-arm` |
| Mac（Intel / Apple Silicon） | `fp-agent-darwin-*` | `fp-cloud-darwin-*` |
| Windows | `fp-agent-windows-amd64.exe` | `fp-cloud-windows-amd64.exe` |

> USB 打印机暂不支持（需 CUPS 桥接），当前面向 IPP 网络打印机。

### 附录 D · 家庭端开机自启配置

**标准 Linux / NAS（systemd）**：

```bash
cat > /etc/systemd/system/fp-agent.service <<'EOF'
[Unit]
Description=FamilyPrint Agent
After=network-online.target
[Service]
ExecStart=/usr/local/bin/fp-agent -cloud wss://print.example.com/ws/agent -token 改成设备token -printer ipp://192.168.1.100:631/ipp/print
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
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
    procd_set_param command /usr/local/bin/fp-agent -cloud wss://print.example.com/ws/agent -token 改成设备token -printer ipp://192.168.1.100:631/ipp/print
    procd_set_param respawn
    procd_close_instance
}
EOF
chmod +x /etc/init.d/fp-agent && /etc/init.d/fp-agent enable && /etc/init.d/fp-agent start
```

### 附录 E · 二进制完整参数

**fp-agent**：

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-cloud` | 云端地址，多个用逗号分隔（换 VPS 平滑切换） | 环境变量 `CLOUD_URLS` |
| `-token` | 设备 token（与云端一致） | 环境变量 `DEVICE_TOKEN` |
| `-printer` | 打印机 IPP 地址 | `ipp://192.168.1.100:631/ipp/print` |
| `-name` | 打印机显示名 | `家里打印机` |
| `-key` | 端到端加密家庭密钥（可选） | 空=不加密 |
| `-mock` | 模拟打印调试 | `0` |

**fp-cloud** 参数见[附录 A](#附录-a--二进制运行云端不用-docker)。

### 附录 F · 从源码构建

```bash
cd cloud-go && go build -o fp-cloud .   # 需 Go 1.22+
cd agent-go && go build -o fp-agent .

bash build-all.sh   # 全平台交叉编译，产出 release/ 目录
```

推送标签 `v*` 后 GitHub Actions 自动编译全部平台并发布：

```bash
git tag v1.0.2 && git push origin v1.0.2
```

### 附录 G · 项目结构

```
familyprint/
├── cloud-go/                 # 云端服务端（含内嵌网页前端）
├── agent-go/                 # 家庭端客户端
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
