# FamilyPrint 家庭远程打印 · 部署指南 v2

架构（对标 cups-yun-agent，入口为网页/H5）：

```
家人浏览器（微信内也可直开）→ https://print.你的域名.com
  → 云端 VPS：FamilyPrint-Cloud（Go 版，账号 + 任务队列 + 文件中转，自动 HTTPS）
      ↕ WSS 出站长连接（家庭端主动连云端，零端口映射/零公网IP/零DDNS）
  → 家庭软路由 Docker：fp-agent + fp-cups
  → 打印机（IPP 网络打印 / USB 经 CUPS）
```

## 一、云端 VPS（约 10 分钟）

1. 把整个 `familyprint/` 目录上传到 VPS
2. 编辑 `docker-compose.cloud.yml`：改 `SECRET_KEY`、`DEVICE_TOKEN`（随机长字符串，记下来）
3. 编辑 `Caddyfile`：改你的备案域名
4. 启动：
   ```bash
   docker compose -f docker-compose.cloud.yml up -d
   ```
5. 验证：浏览器打开 `https://print.你的域名.com` 能看到登录页
6. 默认管理员 `admin / admin123`，**立即登录并给每位家人建账号**

## 二、家庭软路由（约 5 分钟，Go Agent 极简版）

> v2：Agent 已用 Go 重写——单二进制 7MB、运行内存 ~10MB、**无 CUPS 依赖**（IPP 直连网络打印机），
> 家庭端总占用从 ~150MB 降到 ~10MB，适合软路由资源紧张的部署环境。

方式 A（Docker，推荐）：
```bash
DEVICE_TOKEN=与云端一致的token \
PRINTER_IPP_URL=ipp://打印机IP:631/ipp/print \
docker compose -f docker-compose.agent.yml up -d
```

方式 B（不用 Docker，更省）：在任意机器 `cd agent-go && go build -o fp-agent`，
把二进制传到软路由，配 systemd 常驻（见 docker-compose.agent.yml 底部注释）。

验证：云端网页登录后设备自动在线，打印机状态会上报（IPP 探测）。
注：USB 打印机不支持 IPP 直连；如日后要接 USB 打印机，再加装一个 CUPS 容器并让 agent 走 `lp`（可联系开发补充）。

## 三、老人使用（零学习成本）

- 手机浏览器/微信里打开网址 → 登录一次（30 天免密）→ 选文件 → 点"开始打印" → 回家取件
- 微信内打开有自动提示（聊天文件先"保存到手机"再选）；引导"··· → 收藏"
- 大按钮、大字号，全中文

## 四、验证清单

- [ ] 手机流量（非 WiFi）登录上传，打印机出纸，份数/双面正确
- [ ] 重启软路由后 agent 自动重连（云端任务补发不丢）
- [ ] 错误密码 5 次锁定 10 分钟
- [ ] 打印完成后云端与 agent 双侧文件均被删除（隐私）
- [ ] 微信里直开网址可用

## 五、维护

- 日志：`docker logs fp-cloud` / `docker logs fp-agent`
- 全栈 Go：云端 `cloud-go/`（内存 ~15MB）+ 家庭端 `agent-go/`（~8MB），两端均为单二进制，一种语言维护
- 备份：云端 `cloud-data` 卷（账号与记录）
- 更新：替换代码后 `docker compose ... up -d --build`

## 五点五、换 VPS 迁移（约 5 分钟 + DNS 生效时间）

频繁换 VPS 的场景已做专门设计，家人端和软路由**完全不用动**（都走域名）：

1. **提前准备**：域名 DNS 的 TTL 调低到 300 秒（换完生效快）
2. **旧 VPS**：`bash backup.sh` → 得到 `fp-backup-xxx.tar.gz`，下载到本地
3. **新 VPS**：上传项目目录和备份文件 → `bash restore.sh fp-backup-xxx.tar.gz`
4. **DNS**：域名 A 记录改指新 VPS IP
5. **兜底（可选）**：软路由 `CLOUD_URLS` 支持逗号分隔多地址，例如
   `wss://print.你的域名.com/ws/agent,wss://旧VPS-IP直连地址/ws/agent`
   —— DNS 生效间隙 agent 会自动轮询切换，打印不中断

注意：云端只存账号/记录，上传文件打印完即删，迁移内容很小（通常 <1MB）。

## 六、安全要点（已内置）

**端到端加密（E2EE，推荐开启）**
- 打印文件在**浏览器内加密**（AES-256-GCM，密钥由家庭密钥 PBKDF2 派生），经云端中转全程密文，**云端不可读**；家庭端 Agent 用同一密钥解密后再送打印机
- 开启方法：家庭端 Agent 配置环境变量 `PRINT_KEY=你的家庭密钥`；家人浏览器首次打印时弹窗输入同一密钥（存 localStorage，一次即可）
- 密钥不匹配 → 任务失败并提示"家庭密钥不匹配"，不会打出乱码
- 不设置 `PRINT_KEY` 则走明文路径（兼容旧行为）
- 注意：加密文件在记录中显示为 print.enc（文件名也在密文内，保护隐私）

**传输层**
- 家人浏览器 ↔ 云端：HTTPS（Caddy 自动证书）+ HttpOnly SameSite Cookie
- Agent ↔ 云端：**WSS 加密**（经 Caddy 同一证书）；Agent 使用未加密 ws:// 时会打告警日志
- 设备 token 优先走 HTTP 头（`X-Device-Token`），不进 URL，避免被访问日志/代理日志泄露
- token 常数时间比较（防时序攻击）；非法接入在云端日志留痕（含来源 IP）

**接入控制**
- 单设备互斥：新 Agent 连接主动顶替旧连接，杜绝幽灵连接占用下发通道
- Agent 侧纯出站连接，家庭网络零入站端口

**账号与数据**
- 用户口令 pbkdf2-hmac-sha256 哈希存储；登录失败 5 次锁定 10 分钟
- 上传白名单（PDF/JPG/PNG/DOCX/TXT）≤50MB、每日 30 任务、单任务 ≤20 份
- 文件打印完成即删（云端+Agent 双侧），失败任务 24h 兜底清理
- 云端 SQLite 每日备份（backup.sh），数据目录不入 git

**生产部署检查单**
- [ ] 域名走 HTTPS（Caddy 自动）
- [ ] DEVICE_TOKEN 为 64 位随机串且两端一致（`openssl rand -hex 32`）
- [ ] SECRET_KEY 与 DEVICE_TOKEN 不同且均为随机值
- [ ] VPS 防火墙仅放行 80/443（7000 等 frp 端口已废弃无需开放）
- [ ] 定期查看云端日志中的"拒绝非法 Agent 接入"告警

## 七、运行方式与设备支持（v3）

### 双端均支持两种运行方式
| 方式 | 云端 | 家庭端 Agent |
|------|------|-------------|
| Docker | `docker compose -f docker-compose.cloud.yml up -d` | `docker compose -f docker-compose.agent.yml up -d` |
| 二进制 | `./fp-cloud`（systemd 常驻，见下） | `./fp-agent`（systemd 常驻，见 compose 文件注释） |

### 设备/平台支持（Go 交叉编译，`./build-all.sh` 一键产出 `release/`）
| 平台 | Agent | Cloud | 典型设备 |
|------|:---:|:---:|----------|
| linux/amd64 | ✅ | ✅ | x86 软路由、NAS、VPS、旧电脑 |
| linux/arm64 | ✅ | ✅ | 树莓派4/5、ARM 盒子、ARM NAS（N1/R2S/R4S 等） |
| linux/arm (v7) | ✅ | ✅ | 树莓派3、老款 32 位 ARM 设备 |
| darwin/amd64+arm64 | ✅ | ✅ | Mac（Intel/Apple Silicon） |
| windows/amd64 | ✅ | ✅ | Windows 电脑/闲置笔记本 |

### 数据备份（自动识别 Docker 卷 / 本地目录）
- 手动：`bash backup.sh` → 得 `fp-backup-xxx.tar.gz`（<1MB，含账号与记录）；恢复 `bash restore.sh <文件>`
- 建议自动化（crontab，二进制模式同样适用）：
  ```
  0 3 * * * cd /path/to/familyprint && ./backup.sh >> backup.log 2>&1
  ```
- 恢复演练：换 VPS = backup → 新机 restore → 改 DNS，详见"换 VPS 迁移"章节

---

# 📖 完整部署教程（从零到出纸）

> 全程约 30-60 分钟。按顺序做，每一步都有验证点；卡住看文末"故障排查"。

## 第 0 步 · 准备清单

| 需要 | 说明 | 检查方法 |
|------|------|----------|
| 备案域名 | 如 `print.example.com`，A 记录先不急指 | 能登录域名控制台 |
| 公网 VPS | 1核512MB 即可，已备案接入 | `ssh root@VPS_IP` 能连上 |
| 家庭常开设备 | 软路由/NAS/树莓派/旧电脑，能联网 | 能 SSH 或进终端 |
| 网络打印机 | 已接入家庭局域网并固定 IP | 打印机面板查看 IP |

先拿到三个值（后面反复用）：
```bash
# VPS 和家庭端都要用的设备 token（随机生成，记下来）
openssl rand -hex 32
# VPS 用的会话密钥（另一个随机串）
openssl rand -hex 32
# 打印机的 IPP 地址（打印机面板/说明书查询，通常是）
ipp://打印机IP:631/ipp/print
```

## 第 1 步 · 域名解析（2 分钟）

1. 域名控制台 → 添加 A 记录：主机记录 `print`，指向 VPS 的公网 IP
2. 把该记录的 **TTL 改为 300 秒**（日后换 VPS 生效快）
3. 验证（在 VPS 上）：`ping print.example.com` 应解析到 VPS IP

## 第 2 步 · 云端部署（VPS，约 10 分钟）

### 2.1 安装 Docker（已装可跳过）
```bash
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker
```

### 2.2 上传项目
```bash
# 本地执行（或用你习惯的方式上传）
scp -r familyprint/ root@VPS_IP:/opt/familyprint
ssh root@VPS_IP
cd /opt/familyprint
```

### 2.3 配置并启动
```bash
nano docker-compose.cloud.yml
#   SECRET_KEY=改成上面生成的会话密钥
#   DEVICE_TOKEN=改成上面生成的设备 token
nano Caddyfile
#   print.你的域名.com 改成你的真实域名
docker compose -f docker-compose.cloud.yml up -d
```

### 2.4 验证云端 ✅
```bash
docker logs fp-cloud          # 应看到 "FamilyPrint-Cloud (Go) 监听 :8000"
docker logs fp-caddy          # 应看到证书签发成功（约 1-2 分钟）
curl -s https://print.example.com/api/agent-status
# 应返回 {"detail":"未登录"} —— 说明 HTTPS 已通
```

### 2.5 初始化账号（重要）
1. 手机/电脑浏览器打开 `https://print.example.com`
2. 登录默认管理员：`admin / admin123`
3. **立即改密码**（管理员直接改数据库，或用下方 SQL；后续可找我加网页端账号管理）：
   ```bash
   docker exec -it fp-cloud sh   # 若无 sh 则用 fp-cloud 容器的宿主机 sqlite
   ```
   简易方式（宿主机有 sqlite3 时）：
   ```bash
   docker run --rm -it -v fp_cloud-data:/data alpine sh
   # 在容器内：apk add sqlite && sqlite3 /data/app.db
   # 为每位家人插入账号（密码哈希让管理员用一次登录后重置，或联系开发提供生成脚本）
   ```
   > 家庭 5 人以内也可以先共用管理员账号，V1.1 再做账号管理页

## 第 3 步 · 查打印机 IPP 地址（3 分钟）

1. 打印机面板 → 网络设置 → 查看 IP（如 `192.168.1.100`），**在路由器里给它绑定静态 IP**
2. 电脑浏览器访问 `http://192.168.1.100` 能打开打印机管理页（多数品牌支持）
3. IPP 地址通常为：`ipp://192.168.1.100:631/ipp/print`
   - HP 多为 `/ipp/print`，Epson/Canon/Brother 见说明书，或试 `/ipp/printer`
4. 验证（家庭设备上）：
   ```bash
   curl -s -m 3 http://192.168.1.100:631 >/dev/null && echo "打印机 631 端口可达"
   ```

## 第 4 步 · 家庭端 Agent（按设备二选一，约 5 分钟）

### 方式 A · Docker（软路由/NAS 有 Docker 就用这个）
```bash
# 上传 agent-go/ 与 docker-compose.agent.yml 到设备
DEVICE_TOKEN=与云端一致的token PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print docker compose -f docker-compose.agent.yml up -d
docker logs -f fp-agent    # 应看到 "已连接云端 wss://print.example.com/ws/agent"
```

### 方式 B · 裸二进制（无 Docker / 内存极度紧张）
```bash
# 1. 任意有 Go 的机器编译对应平台（或直接用 release/ 里现成的）
cd agent-go && GOOS=linux GOARCH=arm64 go build -o fp-agent .   # 按你的设备选

# 2. 传到设备（假设 /usr/local/bin/）
scp fp-agent root@软路由IP:/usr/local/bin/ && chmod +x /usr/local/bin/fp-agent

# 3. systemd 常驻
cat > /etc/systemd/system/fp-agent.service <<'EOF'
[Unit]
Description=FamilyPrint Agent
After=network-online.target
[Service]
Environment=CLOUD_URLS=wss://print.example.com/ws/agent
Environment=DEVICE_TOKEN=与云端一致
Environment=PRINTER_IPP_URL=ipp://192.168.1.100:631/ipp/print
ExecStart=/usr/local/bin/fp-agent
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now fp-agent
journalctl -u fp-agent -f    # 应看到 "已连接云端"
```

### OpenWrt 设备（procd 代替 systemd）
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

### 验证家庭端 ✅
浏览器登录 `https://print.example.com` → 打印页顶部正常 → 换台设备
`curl -s -b cookie https://print.example.com/api/agent-status` 返回 `{"online":true}`
（最直观：电脑登录后随便传个文件提交，agent 日志出现"任务 #N 收到"）

## 第 5 步 · 全链路验证（5 分钟）

- [ ] 手机**关 WiFi 用流量** → 打开网址 → 登录（30 天免密）
- [ ] 传一个 PDF → 默认参数 → "开始打印" → **打印机出纸**
- [ ] 试改份数=2、双面 → 出纸正确（若双面不生效见排查）
- [ ] 微信里打开网址 → 出现黄色提示卡 → 能正常上传
- [ ] 软路由重启 → agent 自动重连 → 打印机状态恢复"在线"
- [ ] 老人手机：浏览器打开网址 → 菜单"添加到主屏幕" → 以后一点即开

## 第 6 步 · 交给家人（一次性设置）

1. 给每位家人发一条微信，内容照抄：
   > 咱家打印网址：https://print.example.com
   > 账号 xxx 密码 xxx（登录一次以后就不用输了）
   > 用法：点"选择文件"→ 点"开始打印" → 回家取
2. 帮老人首次登录一次 + 添加到主屏幕，之后零辅导

## 🔧 故障排查

| 症状 | 原因与处理 |
|------|-----------|
| 网页打不开 | DNS 未生效（`ping 域名` 核对 IP）；Caddy 证书没签下来（`docker logs fp-caddy`，确认 80/443 放行且域名已备案） |
| agent 连不上云端 | token 两端不一致；域名写错；VPS 防火墙没放行 443 |
| 任务一直"排队中" | agent 离线（`docker logs fp-agent` / `journalctl -u fp-agent`）；打印机离线 |
| 提交后"失败：打印机连接失败" | PRINTER_IPP_URL 写错（先 curl 打印机 631 端口验证）；打印机 IP 变了（绑定静态 IP） |
| 出纸但双面/彩色不生效 | 个别品牌 IPP 属性名有差异，把品牌型号发我，改 agent 的属性写法 |
| 提交"不支持的文件类型" | 目前支持 PDF/JPG/PNG/DOCX/TXT；Word 老格式(.doc)先另存为 .docx 或 PDF |
| 30 天后又要求登录 | 正常，重新登录一次即可；想更久改 SESSION_DAYS 环境变量 |

## 🔄 日常运维速查

| 操作 | 命令 |
|------|------|
| 看云端日志 | `docker logs -f fp-cloud`（二进制：`journalctl -u fp-cloud -f`） |
| 看 agent 日志 | `docker logs -f fp-agent`（二进制：`journalctl -u fp-agent -f`） |
| 备份数据 | `./backup.sh`（<1MB） |
| 升级版本 | 覆盖二进制/代码 → `docker compose ... up -d --build` 或 `systemctl restart fp-agent` |
| 换 VPS | 见"换 VPS 迁移"章节（backup → restore → 改 DNS） |
| 加家人账号 | 目前通过 sqlite 加（见 2.5），网页端账号管理在 V1.1 |

## 八、开源与自动发布（GitHub Actions）

本项目以 **PolyForm Noncommercial 1.0.0** 协议开源——**允许个人、家庭、学习等非商业用途自由使用/修改/分发，禁止商用**（商业授权需另行联系作者）。推送到 GitHub 后，CI 全自动完成构建与发布，无需本地编译环境。

### 自动化流程
| 触发 | 自动执行 |
|------|----------|
| push 到 main | CI：`go vet` + 编译检查（云端/家庭端各一个 job） |
| push 标签 `v*`（如 `git tag v1.0.0 && git push --tags`） | ① 12 个平台的二进制自动编译并挂到 GitHub Release ② 云端/家庭端 Docker 多架构镜像（amd64/arm64/armv7）自动推送到 GHCR |

### 使用 GHCR 镜像部署（推荐，免编译）
```bash
# 首次：fork 或使用本仓库后，把 compose 里的 image 行取消注释并改成你的用户名
# docker-compose.cloud.yml
image: ghcr.io/你的用户名/familyprint-cloud:latest
# docker-compose.agent.yml
image: ghcr.io/你的用户名/familyprint-agent:latest

docker compose -f docker-compose.cloud.yml up -d    # 自动拉取对应架构镜像
```

### 发布新版本（两行命令）
```bash
git tag v1.0.0 && git push origin v1.0.0
# 等 Actions 跑完 → Release 页出现全部二进制；GHCR 出现多架构镜像
```

### 本地提交到 GitHub（首次）
```bash
cd familyprint
git init && git add . && git commit -m "FamilyPrint v1.0 开源发布"
git remote add origin https://github.com/你的用户名/familyprint.git
git push -u origin main
```

> 隐私提示：`.gitignore` 已排除 `data/`、`*.db`（账号库）与 `release/`，家庭成员账号不会入库；推送前请确认 compose/Caddyfile 中的域名与 token 用占位符（仓库内已是占位写法）。
