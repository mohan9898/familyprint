// FamilyPrint-Cloud Go 版：网页入口 + 账号 + 任务队列 + WSS 下发
// 与 Python 版 API/数据库完全兼容，前端零改动；内存 ~15MB 单二进制
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

var (
	secretKey   = envOr("SECRET_KEY", "")
	deviceToken = envOr("DEVICE_TOKEN", "change-me-device-token")
	dataDir     = envOr("DATA_DIR", "data")
	listenAddr  = envOr("LISTEN", ":8000")
	sessionDays = envOr("SESSION_DAYS", "30")
	dailyLimit  = envOr("DAILY_LIMIT", "30")
	jobTimeout  = envOr("JOB_TIMEOUT_MIN", "5") // 单个任务"打印中"最长容忍分钟数
)

// buildVersion：构建期注入，未注入时为 dev（CI 用 -ldflags "-X main.buildVersion=..." 写入）
var buildVersion = "dev"

// 启动安全检查：占位值会让会话签名 / Agent 接入形同虚设。
// 本地调试（监听 127.0.0.1）放行，对外监听则直接拒绝启动，避免带病上线。
func checkSecrets() {
	var problems []string
	if strings.TrimSpace(secretKey) == "" {
		problems = append(problems, "SECRET_KEY / -secret 未设置（会话 Cookie 将无法安全签名）")
	} else if len(secretKey) < 16 {
		problems = append(problems, "SECRET_KEY / -secret 太短（建议 32 位以上：openssl rand -hex 32）")
	}
	if strings.TrimSpace(deviceToken) == "" || deviceToken == "change-me-device-token" {
		problems = append(problems, "DEVICE_TOKEN / -token 仍为默认值（任何人都能冒充家庭端接入）")
	}
	if len(problems) == 0 {
		logDebug("启动配置校验通过")
		return
	}
	local := strings.HasPrefix(listenAddr, "127.0.0.1") || strings.HasPrefix(listenAddr, "localhost")
	if local {
		for _, p := range problems {
			logWarn("%s", p)
		}
		logWarn("当前仅监听本机地址，按调试模式放行；上线前务必修正以上配置")
		return
	}
	for _, p := range problems {
		logError("%s", p)
	}
	logError("启动中止：安全配置不完整。请设置后再启动（仅本地调试可监听 127.0.0.1）")
	os.Exit(1)
}

func init() {
	// 命令行参数（优先于环境变量），支持一条命令直接启动：
	// fp-cloud -secret xxx -token xxx -data /var/lib/familyprint -listen 127.0.0.1:8000
	flag.StringVar(&secretKey, "secret", secretKey, "会话密钥（COOKIE 签名）")
	flag.StringVar(&deviceToken, "token", deviceToken, "设备 token（与家庭端一致）")
	flag.StringVar(&dataDir, "data", dataDir, "数据目录")
	flag.StringVar(&listenAddr, "listen", listenAddr, "监听地址")
	flag.StringVar(&sessionDays, "session-days", sessionDays, "会话有效天数")
	flag.StringVar(&dailyLimit, "daily-limit", dailyLimit, "每用户每日任务上限")
	flag.StringVar(&jobTimeout, "job-timeout", jobTimeout, "任务打印超时分钟数（超时判失败）")
	flag.Parse()
}

// effJobTimeout：任务卡在"打印中"超过该时长即判失败（默认 5 分钟）
func effJobTimeout() time.Duration {
	n, err := strconv.Atoi(jobTimeout)
	if err != nil || n < 1 {
		n = 5
	}
	return time.Duration(n) * time.Minute
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var db *sql.DB
var uploadsDir string

//go:embed static
var staticFS embed.FS

func initDB() {
	uploadsDir = filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0700); err != nil {
		logError("创建数据目录失败: %v（检查目录权限/磁盘是否只读）", err)
		os.Exit(1)
	}
	var err error
	db, err = sql.Open("sqlite", filepath.Join(dataDir, "app.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		logError("打开数据库失败: %v", err)
		os.Exit(1)
	}
	schema := `
	CREATE TABLE IF NOT EXISTS users(
	  id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT UNIQUE NOT NULL,
	  password_hash TEXT NOT NULL, display_name TEXT, is_admin INTEGER DEFAULT 0,
	  failed INTEGER DEFAULT 0, locked_until REAL DEFAULT 0,
	  disabled INTEGER DEFAULT 0, last_login REAL DEFAULT 0, created_at REAL DEFAULT 0,
	  token_version INTEGER DEFAULT 0);
	CREATE TABLE IF NOT EXISTS print_jobs(
	  id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL,
	  file_path TEXT NOT NULL, orig_name TEXT NOT NULL, printer_name TEXT NOT NULL,
	  copies INTEGER DEFAULT 1, color INTEGER DEFAULT 0, sides TEXT DEFAULT 'one-sided',
	  paper TEXT DEFAULT 'A4', status TEXT DEFAULT 'queued', error TEXT,
	  created_at REAL, updated_at REAL, purged INTEGER DEFAULT 0);
	CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT);
	`
	if _, err := db.Exec(schema); err != nil {
		logError("建表失败: %v（数据文件可能损坏，先备份 data/ 再排查）", err)
		os.Exit(1)
	}
	// 老库升级：补齐新增列（已存在则忽略错误）
	added := 0
	for _, ddl := range []string{
		"ALTER TABLE users ADD COLUMN disabled INTEGER DEFAULT 0",
		"ALTER TABLE users ADD COLUMN last_login REAL DEFAULT 0",
		"ALTER TABLE users ADD COLUMN created_at REAL DEFAULT 0",
		"ALTER TABLE users ADD COLUMN token_version INTEGER DEFAULT 0",
	} {
		if _, err := db.Exec(ddl); err == nil {
			added++
		}
	}
	if added > 0 {
		logInfo("数据库结构已升级，补齐 %d 个字段（老数据已保留）", added)
	}
	initAuditTable()
	var n int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n == 0 {
		db.Exec("INSERT INTO users(username,password_hash,display_name,is_admin,created_at) VALUES(?,?,?,1,?)",
			"admin", hashPW("admin123", ""), "管理员", float64(time.Now().Unix()))
		logWarn("已创建默认管理员 admin / admin123 —— 请登录后立即修改密码")
	} else {
		logDebug("账号数据就绪: %d 个账号", n)
	}
}

// ---------- 系统设置（数据库覆盖命令行默认值，管理员可网页修改）----------
func getSetting(k, def string) string {
	var v string
	if db.QueryRow("SELECT v FROM settings WHERE k=?", k).Scan(&v) == nil && v != "" {
		return v
	}
	return def
}

func setSetting(k, v string) {
	db.Exec("INSERT OR REPLACE INTO settings(k,v) VALUES(?,?)", k, v)
}

// 运行时生效值：数据库设置优先，否则用启动参数/环境变量
func effDailyLimit() int {
	n, err := strconv.Atoi(getSetting("daily_limit", dailyLimit))
	if err != nil || n < 0 {
		n = 30
	}
	return n
}

func effSessionDays() int {
	n, err := strconv.Atoi(getSetting("session_days", sessionDays))
	if err != nil || n < 1 {
		n = 30
	}
	return n
}

func effAllowE2EE() bool { return getSetting("allow_e2ee", "1") != "0" }

// ---------- 口令（兼容 Python 版 pbkdf2 格式 salt$hash）----------
func hashPW(pw, salt string) string {
	if salt == "" {
		b := make([]byte, 16)
		rand.Read(b)
		salt = hex.EncodeToString(b)
	}
	_ = sha256.New
	// pbkdf2-hmac-sha256 100k 次
	dk := pbkdf2SHA256([]byte(pw), []byte(salt), 100000, 32)
	return salt + "$" + hex.EncodeToString(dk)
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	var out []byte
	block := 1
	for len(out) < keyLen {
		u := hmacSHA256(salt, append(password, []byte(fmt.Sprintf("%d", block))...))
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			u = hmacSHA256(u, nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
		block++
	}
	return out[:keyLen]
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func checkPW(pw, stored string) bool {
	parts := strings.SplitN(stored, "$", 2)
	if len(parts) != 2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPW(pw, parts[0])), []byte(stored)) == 1
}

// ---------- 会话（签名 Cookie：uid.exp.ver.sig）----------
// ver 为该用户的 token_version，改密码/被管理员强制下线时递增，旧 Cookie 立即失效。
// 兼容旧格式 uid.exp.sig（ver 视为 0），老用户不会被无故登出。
func makeToken(uid, ver int64) string {
	days := effSessionDays()
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	payload := fmt.Sprintf("%d.%d.%d", uid, exp, ver)
	mac := hmacSHA256([]byte(secretKey), []byte(payload))
	return payload + "." + hex.EncodeToString(mac)
}

// parseToken 返回 uid 与 token_version；校验失败返回 (0,0)
func parseToken(token string) (int64, int64) {
	ps := strings.Split(token, ".")
	if len(ps) != 3 && len(ps) != 4 {
		return 0, 0
	}
	sig := ps[len(ps)-1]
	payload := strings.Join(ps[:len(ps)-1], ".")
	mac := hmacSHA256([]byte(secretKey), []byte(payload))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(mac)), []byte(sig)) != 1 {
		return 0, 0
	}
	exp, err := strconv.ParseInt(ps[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, 0
	}
	uid, err := strconv.ParseInt(ps[0], 10, 64)
	if err != nil || uid <= 0 {
		return 0, 0
	}
	var ver int64
	if len(ps) == 4 {
		ver, _ = strconv.ParseInt(ps[2], 10, 64)
	}
	return uid, ver
}

type user struct {
	ID          int64
	Username    string
	DisplayName string
	IsAdmin     int
}

func currentUser(r *http.Request) *user {
	c, err := r.Cookie("fp_session")
	if err != nil {
		return nil
	}
	uid, ver := parseToken(c.Value)
	if uid == 0 {
		return nil
	}
	var name, disp string
	var admin, disabled, tokenVer int
	// 停用账号、或令牌版本落后（已改密码）：已签发的 Cookie 一并失效
	err = db.QueryRow(`SELECT username, COALESCE(display_name,''), is_admin, COALESCE(disabled,0),
		COALESCE(token_version,0) FROM users WHERE id=?`, uid).
		Scan(&name, &disp, &admin, &disabled, &tokenVer)
	if err != nil || disabled == 1 || int64(tokenVer) != ver {
		return nil
	}
	return &user{ID: uid, Username: name, DisplayName: disp, IsAdmin: admin}
}

// ---------- Agent WS 通道 ----------
var (
	agentMu     sync.Mutex
	agentConn   *websocket.Conn
	agentLastAt time.Time // 最近一次确认在线的时刻（连接建立或收到任何消息）
)

// agentOnline 返回是否在线，以及距最后一次在线过去了多久
func agentOnline() (bool, time.Duration) {
	agentMu.Lock()
	defer agentMu.Unlock()
	if agentConn != nil {
		return true, 0
	}
	if agentLastAt.IsZero() {
		return false, 24 * time.Hour
	}
	return false, time.Since(agentLastAt)
}

func sendToAgent(v interface{}) bool {
	agentMu.Lock()
	defer agentMu.Unlock()
	if agentConn == nil {
		return false
	}
	agentConn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return agentConn.WriteJSON(v) == nil
}

func dispatchQueued() {
	rows, err := db.Query("SELECT id, file_path, orig_name, copies, color, sides, paper FROM print_jobs WHERE status='queued' AND purged=0")
	if err != nil {
		return
	}
	type j struct {
		ID                       int64
		Path, Name, Sides, Paper string
		Copies, Color            int
	}
	var jobs []j
	for rows.Next() {
		var x j
		rows.Scan(&x.ID, &x.Path, &x.Name, &x.Copies, &x.Color, &x.Sides, &x.Paper)
		jobs = append(jobs, x)
	}
	rows.Close()
	if len(jobs) == 0 {
		return
	}
	logInfo("家庭端上线，补发 %d 条排队任务", len(jobs))
	for _, job := range jobs {
		data, err := os.ReadFile(job.Path)
		if err != nil {
			// 文件已丢失（被手工删除/磁盘问题）：直接判失败，别让用户在"打印中"里干等
			logError("任务 #%d 文件读取失败: %v，判定为失败", job.ID, err)
			onAgentStatus(job.ID, "failed", "任务文件已丢失，请重新上传打印")
			continue
		}
		ok := sendToAgent(map[string]interface{}{
			"type": "job", "job_id": job.ID, "filename": job.Name,
			"data": base64.StdEncoding.EncodeToString(data),
			"params": map[string]interface{}{
				"copies": job.Copies, "color": job.Color, "sides": job.Sides, "paper": job.Paper}})
		if ok {
			logInfo("任务 #%d 已下发: %s（%dKB）", job.ID, job.Name, len(data)/1024)
		} else {
			logWarn("任务 #%d 下发失败（连接刚断开），等下次上线重试", job.ID)
		}
	}
}

func onAgentStatus(jobID int64, status, errMsg string) {
	var name, old string
	db.QueryRow("SELECT COALESCE(orig_name,''), COALESCE(status,'') FROM print_jobs WHERE id=?", jobID).
		Scan(&name, &old)
	db.Exec("UPDATE print_jobs SET status=?, error=?, updated_at=? WHERE id=?", status, errMsg, time.Now().Unix(), jobID)
	switch status {
	case "completed":
		logInfo("任务 #%d 打印完成: %s", jobID, name)
	case "failed":
		logError("任务 #%d 打印失败: %s — 原因: %s", jobID, name, errMsg)
	case "processing":
		logDebug("任务 #%d 进入打印队列: %s", jobID, name)
	}
	// 打印完成或失败：立即删除云端文件，密文/明文都不留
	if status == "completed" || status == "failed" {
		var path string
		if db.QueryRow("SELECT file_path FROM print_jobs WHERE id=?", jobID).Scan(&path) == nil && path != "" {
			os.Remove(path)
			db.Exec("UPDATE print_jobs SET purged=1 WHERE id=?", jobID)
		}
	}
}

// requeueStaleProcessing：回收「打印中」但长期无进展的任务，解决两个真实问题：
//  1. 家庭端崩溃/断电后再也没上来 → 任务永远停在 processing，用户一直看到「打印中…」
//  2. 大文件或打印队列靠后时，任务确实还在打印 → 不能误判失败
//
// 判定规则：
//   - 家庭端离线且已离线超过 timeout          → 判失败（不会再有人回报了）
//   - 家庭端在线，但任务超过 3×timeout 无回报 → 不改状态，只标记「疑似超时」提示用户去确认
func requeueStaleProcessing(timeout time.Duration) int {
	online, offlineFor := agentOnline()

	// 情况 A：家庭端离线超过阈值 → 这些 processing 任务不可能再有回报，判失败
	if !online && offlineFor > timeout {
		cutoff := float64(time.Now().Add(-timeout).Unix())
		rows, err := db.Query(`SELECT id, COALESCE(file_path,'') FROM print_jobs
			WHERE status='processing' AND purged=0 AND COALESCE(updated_at,created_at,0)<?`, cutoff)
		if err == nil {
			type stale struct {
				id   int64
				path string
			}
			var list []stale
			for rows.Next() {
				var s stale
				rows.Scan(&s.id, &s.path)
				list = append(list, s)
			}
			rows.Close()
			for _, s := range list {
				db.Exec(`UPDATE print_jobs SET status='failed', error=?, updated_at=?, purged=1 WHERE id=?`,
					"家庭端已离线，打印中断（请检查家里电脑/路由器，回来后重新提交）",
					float64(time.Now().Unix()), s.id)
				if s.path != "" {
					os.Remove(s.path)
				}
				logWarn("任务 #%d 因家庭端离线 %s 无进展，已判失败（家庭端已离线 %s）",
					s.id, timeout, offlineFor.Round(time.Second))
			}
			return len(list)
		}
		return 0
	}

	// 情况 B：家庭端在线，但任务太久没回报 → 只加提示，不改状态、不删文件
	hardCutoff := float64(time.Now().Add(-3 * timeout).Unix())
	rows, err := db.Query(`SELECT id FROM print_jobs
		WHERE status='processing' AND purged=0 AND COALESCE(error,'')='' AND COALESCE(updated_at,created_at,0)<?`,
		hardCutoff)
	if err != nil {
		return 0
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		db.Exec(`UPDATE print_jobs SET error=? WHERE id=? AND status='processing'`,
			"似乎卡住了，请到打印机旁确认是否已出纸", id)
		logWarn("任务 #%d 打印已 %s 无进展（家庭端在线），已标注「疑似卡住」提示用户去打印机确认",
			id, (3 * timeout))
	}
	return len(ids)
}

func wsAgent(w http.ResponseWriter, r *http.Request) {
	// 设备 token：优先请求头，兼容查询参数；常数时间比较防时序攻击
	tok := r.Header.Get("X-Device-Token")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(deviceToken)) != 1 {
		logWarn("[%s] 拒绝家庭端接入：设备 token 不匹配（ip=%s，请核对两端 DEVICE_TOKEN 是否一致）",
			ridOf(r), maskIP(clientIP(r)))
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		logWarn("[%s] 家庭端 WebSocket 握手失败: %v", ridOf(r), err)
		return
	}
	agentMu.Lock()
	replaced := agentConn != nil
	if old := agentConn; old != nil {
		old.Close() // 单设备模式：新连接顶替旧连接，防止幽灵连接
	}
	agentConn = conn
	agentLastAt = time.Now()
	agentMu.Unlock()
	if replaced {
		logWarn("[%s] 家庭端重复连接：旧连接已被新连接顶替（ip=%s）", ridOf(r), maskIP(clientIP(r)))
	} else {
		logInfo("家庭端已连接：ip=%s", maskIP(clientIP(r)))
	}
	defer func() {
		agentMu.Lock()
		still := agentConn == conn
		if still {
			agentConn = nil
			agentLastAt = time.Now() // 记下离线时刻，供超时回收判断
		}
		agentMu.Unlock()
		conn.Close()
		logInfo("家庭端已断开%s", map[bool]string{true: "（期间有新连接顶替）", false: ""}[!still])
	}()

	dispatchQueued()
	conn.SetReadLimit(100 << 20)
	for {
		var msg struct {
			Type   string `json:"type"`
			JobID  int64  `json:"job_id"`
			Status string `json:"status"`
			Error  string `json:"error"`
			List   []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"list"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			// 正常断线（浏览器/网络关闭）不打错误，仅 debug；真正的异常才 warn
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				logWarn("家庭端连接异常中断: %v", err)
			} else {
				logDebug("家庭端连接读取结束: %v", err)
			}
			return
		}
		switch msg.Type {
		case "status":
			onAgentStatus(msg.JobID, msg.Status, msg.Error)
		case "printers":
			db.Exec("CREATE TABLE IF NOT EXISTS printers(name TEXT PRIMARY KEY, status TEXT)")
			for _, p := range msg.List {
				// 只在上报状态发生变化时打日志，避免每 30 秒刷一条
				var prev string
				db.QueryRow("SELECT status FROM printers WHERE name=?", p.Name).Scan(&prev)
				if prev != p.Status {
					if p.Status == "offline" {
						logWarn("打印机「%s」离线（检查打印机电源/WiFi/IP 是否变化）", p.Name)
					} else {
						logInfo("打印机「%s」在线", p.Name)
					}
				}
				db.Exec("INSERT OR REPLACE INTO printers(name,status) VALUES(?,?)", p.Name, p.Status)
			}
		}
	}
}

// ---------- HTTP 工具 ----------
func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"detail": msg})
}

func requireUser(w http.ResponseWriter, r *http.Request) *user {
	u := currentUser(r)
	if u == nil {
		jsonErr(w, 401, "未登录")
	}
	return u
}

// ---------- 用户 API（与 Python 版响应结构一致）----------
// normalizeUsername：统一小写去空格。老库中已存在的原样用户名仍可登录（下面做了兜底查询）。
func normalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Password string }
	json.NewDecoder(r.Body).Decode(&body)
	uname := normalizeUsername(body.Username)
	if uname == "" || body.Password == "" {
		jsonErr(w, 400, "请输入用户名和密码")
		return
	}
	var id int64
	var hash, disp string
	var admin, failed, disabled int
	var locked float64
	var tokenVer int64
	// 先按规范化用户名查；查不到再按原样查（兼容升级前创建的大写用户名）
	err := db.QueryRow(`SELECT id, password_hash, COALESCE(display_name,''), is_admin, failed,
		locked_until, COALESCE(disabled,0), COALESCE(token_version,0) FROM users WHERE username=?`,
		uname).Scan(&id, &hash, &disp, &admin, &failed, &locked, &disabled, &tokenVer)
	if err == sql.ErrNoRows && body.Username != uname {
		err = db.QueryRow(`SELECT id, password_hash, COALESCE(display_name,''), is_admin, failed,
			locked_until, COALESCE(disabled,0), COALESCE(token_version,0) FROM users WHERE username=?`,
			strings.TrimSpace(body.Username)).Scan(&id, &hash, &disp, &admin, &failed, &locked, &disabled, &tokenVer)
	}
	if err != nil {
		logReq(r, lvWarn, "登录失败: 用户名 %q 不存在（ip=%s）", uname, maskIP(clientIP(r)))
		jsonErr(w, 401, "用户名或密码错误")
		return
	}
	if disabled == 1 {
		logReq(r, lvWarn, "登录被拒: %q 账号已停用（ip=%s）", uname, maskIP(clientIP(r)))
		jsonErr(w, 403, "该账号已被管理员停用")
		return
	}
	if locked > float64(time.Now().Unix()) {
		logReq(r, lvWarn, "登录被拒: %q 处于锁定中，剩余 %d 分钟（ip=%s）",
			uname, int((locked-float64(time.Now().Unix()))/60)+1, maskIP(clientIP(r)))
		jsonErr(w, 429, "失败次数过多，请 10 分钟后再试")
		return
	}
	if !checkPW(body.Password, hash) {
		failed++
		newLocked := 0.0
		if failed >= 5 {
			newLocked = float64(time.Now().Add(10 * time.Minute).Unix())
			logReq(r, lvWarn, "登录失败: %q 密码错误 %d 次，已锁定 10 分钟（ip=%s）",
				uname, failed, maskIP(clientIP(r)))
		} else {
			logReq(r, lvWarn, "登录失败: %q 密码错误（第 %d/5 次，ip=%s）",
				uname, failed, maskIP(clientIP(r)))
		}
		db.Exec("UPDATE users SET failed=?, locked_until=? WHERE id=?", failed, newLocked, id)
		jsonErr(w, 401, "用户名或密码错误")
		return
	}
	now := float64(time.Now().Unix())
	db.Exec("UPDATE users SET failed=0, locked_until=0, last_login=? WHERE id=?", now, id)
	days := effSessionDays()
	// Secure 属性：仅 HTTPS 下才强制。纯 HTTP（内网 IP 直连）访问时若带 Secure，
	// 浏览器会直接丢弃该 cookie，表现为「登录请求 200 但后续接口全 401、页面不跳转」。
	// 用 COOKIE_INSECURE=1 显式放行 HTTP；否则按请求实际协议自动判断。
	secure := os.Getenv("COOKIE_INSECURE") != "1" && requestIsHTTPS(r)
	if os.Getenv("COOKIE_INSECURE") != "1" && !secure {
		logReq(r, lvWarn, "检测到 HTTP 明文访问，会话 cookie 已放开 Secure 限制。"+
			"内网自用可接受，公网部署请务必配置 HTTPS 反向代理")
	}
	http.SetCookie(w, &http.Cookie{
		Name: "fp_session", Value: makeToken(id, tokenVer), Path: "/",
		MaxAge: days * 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure})
	logReq(r, lvInfo, "登录成功: %q（id=%d，管理员=%v，记住 %d 天，ip=%s）",
		uname, id, admin == 1, days, maskIP(clientIP(r)))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id, "username": uname, "display_name": disp, "is_admin": admin})
}

func apiLogout(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u != nil {
		logReq(r, lvInfo, "退出登录: %q", u.Username)
	}
	http.SetCookie(w, &http.Cookie{Name: "fp_session", Value: "", Path: "/", MaxAge: -1})
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func apiMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		jsonErr(w, 401, "未登录")
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": u.ID, "username": u.Username, "display_name": u.DisplayName, "is_admin": u.IsAdmin})
}

func apiPrinters(w http.ResponseWriter, r *http.Request) {
	if requireUser(w, r) == nil {
		return
	}
	type pr struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	var list []pr
	rows, err := db.Query("SELECT name, status FROM printers")
	if err == nil {
		for rows.Next() {
			var p pr
			rows.Scan(&p.Name, &p.Status)
			list = append(list, p)
		}
		rows.Close()
	}
	if len(list) == 0 {
		list = []pr{{Name: "家里打印机", Status: "online"}}
	}
	json.NewEncoder(w).Encode(list)
}

var allowedExt = map[string]bool{".pdf": true, ".jpg": true, ".jpeg": true, ".png": true, ".docx": true, ".txt": true, ".enc": true} // .enc = 端到端加密文件

// displayName：列表展示用的文件名。端到端加密的密文统一叫 print.enc，
// 对家人没有意义；上传时网页端把真实文件名放在了设置摘要之外无法保留，
// 因此密文任务显示友好的「加密文件」占位（解密后的原名只有家庭端能看到，
// 由家庭端解密后上报日志；列表里不展示明文原名也避免云端泄露元数据）。
func displayName(name string) string {
	if strings.EqualFold(filepath.Ext(name), ".enc") {
		return "加密文档"
	}
	return name
}

func apiCreateJob(w http.ResponseWriter, r *http.Request) {
	u := requireUser(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 52<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		logReq(r, lvWarn, "上传被拒: 表单解析失败（可能超过 50MB 上限）: %v", err)
		jsonErr(w, 400, "文件超过 50MB 上限")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		logReq(r, lvWarn, "上传被拒: 请求里没有文件")
		jsonErr(w, 400, "缺少文件")
		return
	}
	defer file.Close()
	// 净化文件名：只取 basename，去掉路径分隔符与控制字符，避免展示/落盘时出问题
	origName := filepath.Base(strings.ReplaceAll(hdr.Filename, "\\", "/"))
	origName = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, origName)
	if origName == "" || origName == "." || origName == ".." || origName == "/" {
		origName = "print" + filepath.Ext(hdr.Filename)
	}
	if len(origName) > 120 {
		origName = origName[:100] + filepath.Ext(origName)
	}
	ext := strings.ToLower(filepath.Ext(origName))
	if !allowedExt[ext] {
		logReq(r, lvWarn, "上传被拒: 不支持的类型 %q（文件 %q）", ext, origName)
		jsonErr(w, 400, "不支持的文件类型，仅支持：.docx, .jpg, .jpeg, .pdf, .png, .txt")
		return
	}
	if ext == ".enc" && !effAllowE2EE() {
		logReq(r, lvWarn, "上传被拒: %q 要求端到端加密，但管理员已关闭该功能", u.Username)
		jsonErr(w, 400, "管理员已关闭端到端加密上传，请关闭加密后重试")
		return
	}
	printer := r.FormValue("printer")
	if printer == "" {
		printer = "家里打印机"
	}
	copies, _ := strconv.Atoi(r.FormValue("copies"))
	if copies < 1 {
		copies = 1
	}
	if copies > 20 {
		copies = 20
	}
	color, _ := strconv.Atoi(r.FormValue("color"))
	sides := r.FormValue("sides")
	if sides == "" {
		sides = "one-sided"
	}
	paper := r.FormValue("paper")
	if paper == "" {
		paper = "A4"
	}
	// 每日限额（管理员可在网页端调整，实时生效）
	limit := effDailyLimit()
	var n int
	db.QueryRow("SELECT COUNT(*) FROM print_jobs WHERE user_id=? AND created_at>?", u.ID, float64(time.Now().Add(-24*time.Hour).Unix())).Scan(&n)
	if n >= limit {
		logReq(r, lvWarn, "上传被拒: %q 已达今日上限 %d 个任务", u.Username, limit)
		jsonErr(w, 429, "已达今日打印次数上限")
		return
	}
	rnd := make([]byte, 8)
	rand.Read(rnd)
	fpath := filepath.Join(uploadsDir, hex.EncodeToString(rnd)+ext)
	out, err := os.Create(fpath)
	if err != nil {
		logReq(r, lvError, "任务保存失败（落盘出错）: %v", err)
		jsonErr(w, 500, "存储失败")
		return
	}
	size, _ := io.Copy(out, file)
	out.Close()
	if size > 50<<20 {
		os.Remove(fpath)
		logReq(r, lvWarn, "上传被拒: 文件 %q 实际 %dMB，超过 50MB", origName, size/1048576)
		jsonErr(w, 400, "文件超过 50MB 上限")
		return
	}
	now := float64(time.Now().Unix())
	res, err := db.Exec("INSERT INTO print_jobs(user_id,file_path,orig_name,printer_name,copies,color,sides,paper,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'queued',?,?)",
		u.ID, fpath, origName, printer, copies, color, sides, paper, now, now)
	if err != nil {
		os.Remove(fpath)
		logReq(r, lvError, "任务入库失败: %v", err)
		jsonErr(w, 500, "任务创建失败")
		return
	}
	jid, _ := res.LastInsertId()
	data, _ := os.ReadFile(fpath)
	sent := sendToAgent(map[string]interface{}{
		"type": "job", "job_id": jid, "filename": origName,
		"data": base64.StdEncoding.EncodeToString(data),
		"params": map[string]interface{}{
			"copies": copies, "color": color, "sides": sides, "paper": paper}})
	// 用户侧"提交"日志：谁、什么文件、多大、几份、是否即时下发
	logReq(r, lvInfo, "新任务 #%d: %q 用户=%q %dKB %d份 %s %s 即时下发=%v",
		jid, origName, u.Username, size/1024, copies,
		map[int]string{1: "彩色", 0: "黑白"}[color],
		map[bool]string{true: "双面", false: "单面"}[sides == "two-sided"], sent)
	if !sent {
		logReq(r, lvInfo, "任务 #%d 已排队，等家庭端上线后自动下发", jid)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"id": jid, "status": "queued", "agent_online": sent})
}

func apiListJobs(w http.ResponseWriter, r *http.Request) {
	u := requireUser(w, r)
	if u == nil {
		return
	}
	rows, err := db.Query("SELECT id,orig_name,printer_name,copies,color,sides,paper,status,COALESCE(error,''),created_at FROM print_jobs WHERE user_id=? ORDER BY id DESC LIMIT 50", u.ID)
	if err != nil {
		logReq(r, lvError, "查询打印记录失败: %v", err)
		jsonErr(w, 500, "查询失败")
		return
	}
	defer rows.Close()
	jobs := []map[string]interface{}{}
	for rows.Next() {
		var id, copies, color int64
		var name, printerName, sides, paper, status, errMsg, created string
		var createdRaw float64
		rows.Scan(&id, &name, &printerName, &copies, &color, &sides, &paper, &status, &errMsg, &createdRaw)
		created = time.Unix(int64(createdRaw), 0).Format("01-02 15:04")
		jobs = append(jobs, map[string]interface{}{
			"id": id, "orig_name": displayName(name), "printer_name": printerName, "copies": copies,
			"color": color, "sides": sides, "paper": paper, "status": status,
			"error": errMsg, "created_at": created})
	}
	json.NewEncoder(w).Encode(jobs)
}

func apiCancelJob(w http.ResponseWriter, r *http.Request) {
	u := requireUser(w, r)
	if u == nil {
		return
	}
	var jid int64
	fmt.Sscanf(r.PathValue("id"), "%d", &jid)
	var status, fpath string
	err := db.QueryRow("SELECT status, file_path FROM print_jobs WHERE id=? AND user_id=?", jid, u.ID).Scan(&status, &fpath)
	if err != nil || status != "queued" {
		logReq(r, lvWarn, "取消任务 #%d 被拒: 当前状态 %q（仅排队中可取消）", jid, status)
		jsonErr(w, 400, "仅排队中的任务可取消")
		return
	}
	db.Exec("UPDATE print_jobs SET status='canceled', updated_at=? WHERE id=?", float64(time.Now().Unix()), jid)
	os.Remove(fpath)
	sendToAgent(map[string]interface{}{"type": "cancel", "job_id": jid})
	logReq(r, lvInfo, "用户 %q 取消任务 #%d", u.Username, jid)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func apiAgentStatus(w http.ResponseWriter, r *http.Request) {
	if requireUser(w, r) == nil {
		return
	}
	agentMu.Lock()
	on := agentConn != nil
	agentMu.Unlock()
	json.NewEncoder(w).Encode(map[string]bool{"online": on})
}

// ---------- 管理员 API ----------
// requireAdmin：先校验登录，再校验管理员身份；普通用户返回 403
func requireAdmin(w http.ResponseWriter, r *http.Request) *user {
	u := currentUser(r)
	if u == nil {
		jsonErr(w, 401, "未登录")
		return nil
	}
	if u.IsAdmin != 1 {
		logReq(r, lvWarn, "越权访问被拒: 用户 %q 尝试访问管理员接口 %s", u.Username, r.URL.Path)
		jsonErr(w, 403, "需要管理员权限")
		return nil
	}
	return u
}

func jsonOK(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// 账号列表
func apiAdminUsers(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	rows, err := db.Query(`SELECT id, username, COALESCE(display_name,''), is_admin,
		COALESCE(disabled,0), COALESCE(last_login,0), COALESCE(created_at,0) FROM users ORDER BY id`)
	if err != nil {
		jsonErr(w, 500, "查询失败")
		return
	}
	defer rows.Close()
	list := []map[string]interface{}{}
	for rows.Next() {
		var id, admin, disabled int64
		var uname, disp string
		var lastLogin, createdAt float64
		rows.Scan(&id, &uname, &disp, &admin, &disabled, &lastLogin, &createdAt)
		var jobs int64
		db.QueryRow("SELECT COUNT(*) FROM print_jobs WHERE user_id=?", id).Scan(&jobs)
		list = append(list, map[string]interface{}{
			"id": id, "username": uname, "display_name": disp, "is_admin": admin,
			"disabled": disabled, "last_login": lastLogin, "created_at": createdAt,
			"job_count": jobs})
	}
	json.NewEncoder(w).Encode(list)
}

// 新增成员
func apiAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	me := requireAdmin(w, r)
	if me == nil {
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		IsAdmin     int    `json:"is_admin"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	uname := normalizeUsername(body.Username)
	if len(uname) < 2 || len(uname) > 32 {
		jsonErr(w, 400, "用户名长度需为 2-32 位")
		return
	}
	if strings.ContainsAny(uname, " \t\r\n/@") {
		jsonErr(w, 400, "用户名不能包含空格或特殊字符")
		return
	}
	if len(body.Password) < 6 {
		jsonErr(w, 400, "密码至少 6 位")
		return
	}
	// 大小写不敏感查重（老库可能存有大写用户名）
	var dup int
	db.QueryRow("SELECT COUNT(*) FROM users WHERE LOWER(username)=?", uname).Scan(&dup)
	if dup > 0 {
		jsonErr(w, 400, "用户名已存在")
		return
	}
	isAdmin := 0
	if body.IsAdmin == 1 {
		isAdmin = 1
	}
	res, err := db.Exec(`INSERT INTO users(username,password_hash,display_name,is_admin,created_at,token_version)
		VALUES(?,?,?,?,?,0)`,
		uname, hashPW(body.Password, ""), strings.TrimSpace(body.DisplayName), isAdmin, float64(time.Now().Unix()))
	if err != nil {
		jsonErr(w, 400, "用户名已存在")
		return
	}
	id, _ := res.LastInsertId()
	audit(r, me.ID, me.Username, "新增成员", uname,
		fmt.Sprintf("显示名=%q 管理员=%v", strings.TrimSpace(body.DisplayName), isAdmin == 1))
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": id})
}

// 重置密码：同时递增 token_version，让该用户所有已登录设备立即失效
func apiAdminResetPW(w http.ResponseWriter, r *http.Request) {
	me := requireAdmin(w, r)
	if me == nil {
		return
	}
	var uid int64
	fmt.Sscanf(r.PathValue("id"), "%d", &uid)
	var body struct {
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if len(body.Password) < 6 {
		jsonErr(w, 400, "密码至少 6 位")
		return
	}
	res, err := db.Exec(`UPDATE users SET password_hash=?, failed=0, locked_until=0,
		token_version=COALESCE(token_version,0)+1 WHERE id=?`,
		hashPW(body.Password, ""), uid)
	if n, _ := res.RowsAffected(); err != nil || n == 0 {
		jsonErr(w, 404, "用户不存在")
		return
	}
	var target string
	db.QueryRow("SELECT username FROM users WHERE id=?", uid).Scan(&target)
	audit(r, me.ID, me.Username, "重置密码", target, "该用户所有已登录设备立即失效")
	jsonOK(w)
}

// 统计「其他可用管理员」数量（启用中的管理员，不含指定 id）
func otherActiveAdmins(excludeID int64) int {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin=1 AND COALESCE(disabled,0)=0 AND id<>?`,
		excludeID).Scan(&n)
	return n
}

// 启用 / 停用
func apiAdminToggleUser(w http.ResponseWriter, r *http.Request) {
	me := requireAdmin(w, r)
	if me == nil {
		return
	}
	var uid int64
	fmt.Sscanf(r.PathValue("id"), "%d", &uid)
	if uid == me.ID {
		jsonErr(w, 400, "不能停用自己的账号")
		return
	}
	var body struct {
		Disabled int `json:"disabled"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	v := 0
	if body.Disabled == 1 {
		v = 1
	}
	// 防锁死：停用的若是一个管理员，且没有其他可用管理员，则拒绝
	if v == 1 {
		var isAdmin, disabled int
		if db.QueryRow("SELECT is_admin, COALESCE(disabled,0) FROM users WHERE id=?", uid).
			Scan(&isAdmin, &disabled) != nil {
			jsonErr(w, 404, "用户不存在")
			return
		}
		if isAdmin == 1 && disabled == 0 && otherActiveAdmins(uid) == 0 {
			jsonErr(w, 400, "这是最后一个可用的管理员，不能停用")
			return
		}
	}
	if _, err := db.Exec("UPDATE users SET disabled=? WHERE id=?", v, uid); err != nil {
		logReq(r, lvError, "停用/启用成员失败: %v", err)
		jsonErr(w, 500, "操作失败")
		return
	}
	var target string
	db.QueryRow("SELECT username FROM users WHERE id=?", uid).Scan(&target)
	audit(r, me.ID, me.Username, map[int]string{1: "停用成员", 0: "启用成员"}[v], target, "")
	jsonOK(w)
}

// 删除成员（连带清理其打印记录与残留文件）
func apiAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	me := requireAdmin(w, r)
	if me == nil {
		return
	}
	var uid int64
	fmt.Sscanf(r.PathValue("id"), "%d", &uid)
	if uid == me.ID {
		jsonErr(w, 400, "不能删除自己的账号")
		return
	}
	// 防锁死：删除的若是最后一个可用管理员，则拒绝
	var isAdmin, disabled int
	if db.QueryRow("SELECT is_admin, COALESCE(disabled,0) FROM users WHERE id=?", uid).
		Scan(&isAdmin, &disabled) != nil {
		jsonErr(w, 404, "用户不存在")
		return
	}
	if isAdmin == 1 && disabled == 0 && otherActiveAdmins(uid) == 0 {
		jsonErr(w, 400, "这是最后一个可用的管理员，不能删除")
		return
	}
	rows, err := db.Query("SELECT file_path FROM print_jobs WHERE user_id=?", uid)
	if err == nil {
		var paths []string
		for rows.Next() {
			var p string
			rows.Scan(&p)
			paths = append(paths, p)
		}
		rows.Close()
		for _, p := range paths {
			os.Remove(p)
		}
	}
	db.Exec("DELETE FROM print_jobs WHERE user_id=?", uid)
	var target string
	db.QueryRow("SELECT username FROM users WHERE id=?", uid).Scan(&target)
	if _, err := db.Exec("DELETE FROM users WHERE id=?", uid); err != nil {
		logReq(r, lvError, "删除成员失败: %v", err)
		jsonErr(w, 500, "删除失败")
		return
	}
	audit(r, me.ID, me.Username, "删除成员", target, "已连带清理其打印记录与文件")
	jsonOK(w)
}

// 全部打印记录（最近 200 条，可按用户过滤）
func apiAdminJobs(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	q := `SELECT j.id, j.user_id, COALESCE(u.username,'-'), j.orig_name, j.printer_name,
		j.copies, j.color, j.sides, j.paper, j.status, COALESCE(j.error,''), j.created_at
		FROM print_jobs j LEFT JOIN users u ON u.id=j.user_id`
	args := []interface{}{}
	if uid := r.URL.Query().Get("user_id"); uid != "" {
		q += " WHERE j.user_id=?"
		args = append(args, uid)
	}
	q += " ORDER BY j.id DESC LIMIT 200"
	rows, err := db.Query(q, args...)
	if err != nil {
		jsonErr(w, 500, "查询失败")
		return
	}
	defer rows.Close()
	jobs := []map[string]interface{}{}
	for rows.Next() {
		var id, uid, copies, color int64
		var uname, name, printerName, sides, paper, status, errMsg string
		var createdRaw float64
		rows.Scan(&id, &uid, &uname, &name, &printerName, &copies, &color, &sides, &paper, &status, &errMsg, &createdRaw)
		jobs = append(jobs, map[string]interface{}{
			"id": id, "user_id": uid, "username": uname, "orig_name": name,
			"printer_name": printerName, "copies": copies, "color": color,
			"sides": sides, "paper": paper, "status": status, "error": errMsg,
			"created_at": time.Unix(int64(createdRaw), 0).Format("2006-01-02 15:04")})
	}
	json.NewEncoder(w).Encode(jobs)
}

// 设备状态
func apiAdminAgent(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	agentMu.Lock()
	on := agentConn != nil
	agentMu.Unlock()
	var total, today int64
	db.QueryRow("SELECT COUNT(*) FROM print_jobs").Scan(&total)
	db.QueryRow("SELECT COUNT(*) FROM print_jobs WHERE created_at>?", float64(time.Now().Add(-24*time.Hour).Unix())).Scan(&today)
	var printers []map[string]string
	rows, err := db.Query("SELECT name, status FROM printers")
	if err == nil {
		for rows.Next() {
			var n, s string
			rows.Scan(&n, &s)
			printers = append(printers, map[string]string{"name": n, "status": s})
		}
		rows.Close()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"online": on, "total_jobs": total, "jobs_24h": today, "printers": printers})
}

// 强制断开局域网设备连接（家庭端会自动重连）
func apiAdminKickAgent(w http.ResponseWriter, r *http.Request) {
	me := requireAdmin(w, r)
	if me == nil {
		return
	}
	agentMu.Lock()
	had := agentConn != nil
	if agentConn != nil {
		agentConn.Close()
		agentConn = nil
		agentLastAt = time.Now()
	}
	agentMu.Unlock()
	if had {
		audit(r, me.ID, me.Username, "强制断开家庭端", "agent", "家庭端将在 30 秒内自动重连")
	} else {
		logReq(r, lvWarn, "管理员请求断开家庭端，但当前本来就未连接")
	}
	jsonOK(w)
}

// 读取系统设置
func apiAdminGetSettings(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"daily_limit": effDailyLimit(), "session_days": effSessionDays(),
		"allow_e2ee": effAllowE2EE()})
}

// 保存系统设置
func apiAdminPutSettings(w http.ResponseWriter, r *http.Request) {
	me := requireAdmin(w, r)
	if me == nil {
		return
	}
	var body struct {
		DailyLimit  *int  `json:"daily_limit"`
		SessionDays *int  `json:"session_days"`
		AllowE2EE   *bool `json:"allow_e2ee"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	before := fmt.Sprintf("每日%s/会话%s天/加密%s",
		strconv.Itoa(effDailyLimit()), strconv.Itoa(effSessionDays()),
		map[bool]string{true: "开", false: "关"}[effAllowE2EE()])
	if body.DailyLimit != nil {
		if *body.DailyLimit < 1 || *body.DailyLimit > 999 {
			jsonErr(w, 400, "每日上限需在 1-999 之间")
			return
		}
		setSetting("daily_limit", strconv.Itoa(*body.DailyLimit))
	}
	if body.SessionDays != nil {
		if *body.SessionDays < 1 || *body.SessionDays > 365 {
			jsonErr(w, 400, "会话天数需在 1-365 之间")
			return
		}
		setSetting("session_days", strconv.Itoa(*body.SessionDays))
	}
	if body.AllowE2EE != nil {
		if *body.AllowE2EE {
			setSetting("allow_e2ee", "1")
		} else {
			setSetting("allow_e2ee", "0")
		}
	}
	after := fmt.Sprintf("每日%s/会话%s天/加密%s",
		strconv.Itoa(effDailyLimit()), strconv.Itoa(effSessionDays()),
		map[bool]string{true: "开", false: "关"}[effAllowE2EE()])
	if before != after {
		audit(r, me.ID, me.Username, "修改系统设置", "settings", before+" → "+after)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "daily_limit": effDailyLimit(), "session_days": effSessionDays(),
		"allow_e2ee": effAllowE2EE()})
}

// 审计日志查询（管理后台「记录」页展示最近操作）
func apiAdminAudit(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	json.NewEncoder(w).Encode(loadAudit(limit))
}

// 后台看护循环：
//   - 每 20 秒回收卡死的 processing 任务（默认超时 5 分钟，可用 -job-timeout 调整）
//   - 每小时清理完成/取消超 24h 的残留文件
func cleanupLoop() {
	// 每小时清理完成/取消超 24h 的残留文件 + 清理 90 天前审计日志
	go func() {
		for {
			time.Sleep(time.Hour)
			pruneAudit()
		}
	}()
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if n := requeueStaleProcessing(effJobTimeout()); n > 0 {
				logDebug("本轮回收卡死任务 %d 条", n)
			}
		}
	}()
	for {
		time.Sleep(time.Hour)
		rows, err := db.Query(`SELECT file_path FROM print_jobs
			WHERE status IN ('completed','canceled','failed') AND coalesce(purged,0)=0 AND updated_at<?`,
			float64(time.Now().Add(-24*time.Hour).Unix()))
		if err != nil {
			logError("清理残留文件失败: %v", err)
			continue
		}
		var paths []string
		for rows.Next() {
			var p string
			rows.Scan(&p)
			paths = append(paths, p)
		}
		rows.Close()
		for _, p := range paths {
			if p != "" {
				os.Remove(p)
			}
		}
		db.Exec(`UPDATE print_jobs SET purged=1 WHERE status IN ('completed','canceled','failed')
			AND coalesce(purged,0)=0 AND updated_at<?`, float64(time.Now().Add(-24*time.Hour).Unix()))
		if len(paths) > 0 {
			logInfo("清理 24 小时前的残留文件 %d 个", len(paths))
		}
	}
}

func main() {
	initLogLevel()
	checkSecrets()
	initDB()
	go cleanupLoop()
	logSelfCheck()
	logStartupBanner()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", apiLogin)
	mux.HandleFunc("POST /api/logout", apiLogout)
	mux.HandleFunc("GET /api/me", apiMe)
	mux.HandleFunc("GET /api/printers", apiPrinters)
	mux.HandleFunc("POST /api/print-jobs", apiCreateJob)
	mux.HandleFunc("GET /api/print-jobs", apiListJobs)
	mux.HandleFunc("POST /api/print-jobs/{id}/cancel", apiCancelJob)
	mux.HandleFunc("GET /api/agent-status", apiAgentStatus)
	// 管理员
	mux.HandleFunc("GET /api/admin/users", apiAdminUsers)
	mux.HandleFunc("POST /api/admin/users", apiAdminCreateUser)
	mux.HandleFunc("PUT /api/admin/users/{id}/password", apiAdminResetPW)
	mux.HandleFunc("PUT /api/admin/users/{id}/status", apiAdminToggleUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", apiAdminDeleteUser)
	mux.HandleFunc("GET /api/admin/jobs", apiAdminJobs)
	mux.HandleFunc("GET /api/admin/agent", apiAdminAgent)
	mux.HandleFunc("POST /api/admin/agent/kick", apiAdminKickAgent)
	mux.HandleFunc("GET /api/admin/audit", apiAdminAudit)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, r *http.Request) {
		// 健康检查：反代/监控用，不打业务日志（中间件里静态路径已降级）
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/admin/settings", apiAdminGetSettings)
	mux.HandleFunc("PUT /api/admin/settings", apiAdminPutSettings)
	mux.HandleFunc("/ws/agent", wsAgent)
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(staticSub)))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           withLog(mux), // 全局访问日志 + 请求 ID
		ReadHeaderTimeout: 15 * time.Second,
	}
	logInfo("开始监听 %s（Ctrl+C 停止）", listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logError("监听 %s 失败: %v（端口被占用？权限不足？）", listenAddr, err)
		os.Exit(1)
	}
	logInfo("服务已停止")
}
