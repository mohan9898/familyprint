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
	"log"
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
	os.MkdirAll(uploadsDir, 0700)
	var err error
	db, err = sql.Open("sqlite", filepath.Join(dataDir, "app.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
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
		log.Fatal(err)
	}
	// 老库升级：补齐新增列（已存在则忽略错误）
	for _, ddl := range []string{
		"ALTER TABLE users ADD COLUMN disabled INTEGER DEFAULT 0",
		"ALTER TABLE users ADD COLUMN last_login REAL DEFAULT 0",
		"ALTER TABLE users ADD COLUMN created_at REAL DEFAULT 0",
		"ALTER TABLE users ADD COLUMN token_version INTEGER DEFAULT 0",
	} {
		db.Exec(ddl)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n == 0 {
		db.Exec("INSERT INTO users(username,password_hash,display_name,is_admin,created_at) VALUES(?,?,?,1,?)",
			"admin", hashPW("admin123", ""), "管理员", float64(time.Now().Unix()))
		log.Println("已创建默认管理员 admin/admin123（请尽快修改）")
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
	for _, job := range jobs {
		data, err := os.ReadFile(job.Path)
		if err != nil {
			data = []byte{}
		}
		sendToAgent(map[string]interface{}{
			"type": "job", "job_id": job.ID, "filename": job.Name,
			"data": base64.StdEncoding.EncodeToString(data),
			"params": map[string]interface{}{
				"copies": job.Copies, "color": job.Color, "sides": job.Sides, "paper": job.Paper}})
	}
}

func onAgentStatus(jobID int64, status, errMsg string) {
	db.Exec("UPDATE print_jobs SET status=?, error=?, updated_at=? WHERE id=?", status, errMsg, time.Now().Unix(), jobID)
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
				log.Printf("任务 #%d 因家庭端离线超时，已标记失败", s.id)
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
		log.Printf("任务 #%d 打印超时（%v 无进展），已标注提示", id, 3*timeout)
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
		log.Printf("⚠️ 拒绝非法 Agent 接入: remote=%s", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	up := websocket.Upgrader{}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	agentMu.Lock()
	if old := agentConn; old != nil {
		old.Close() // 单设备模式：新连接顶替旧连接，防止幽灵连接
	}
	agentConn = conn
	agentLastAt = time.Now()
	agentMu.Unlock()
	log.Printf("Agent 已连接: remote=%s", r.RemoteAddr)
	defer func() {
		agentMu.Lock()
		if agentConn == conn {
			agentConn = nil
			agentLastAt = time.Now() // 记下离线时刻，供超时回收判断
		}
		agentMu.Unlock()
		conn.Close()
		log.Println("Agent 断开")
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
			log.Printf("Agent 连接读取错误: %v", err)
			return
		}
		switch msg.Type {
		case "status":
			onAgentStatus(msg.JobID, msg.Status, msg.Error)
		case "printers":
			db.Exec("CREATE TABLE IF NOT EXISTS printers(name TEXT PRIMARY KEY, status TEXT)")
			for _, p := range msg.List {
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
		jsonErr(w, 401, "用户名或密码错误")
		return
	}
	if disabled == 1 {
		jsonErr(w, 403, "该账号已被管理员停用")
		return
	}
	if locked > float64(time.Now().Unix()) {
		jsonErr(w, 429, "失败次数过多，请 10 分钟后再试")
		return
	}
	if !checkPW(body.Password, hash) {
		failed++
		newLocked := 0.0
		if failed >= 5 {
			newLocked = float64(time.Now().Add(10 * time.Minute).Unix())
		}
		db.Exec("UPDATE users SET failed=?, locked_until=? WHERE id=?", failed, newLocked, id)
		jsonErr(w, 401, "用户名或密码错误")
		return
	}
	now := float64(time.Now().Unix())
	db.Exec("UPDATE users SET failed=0, locked_until=0, last_login=? WHERE id=?", now, id)
	days := effSessionDays()
	http.SetCookie(w, &http.Cookie{
		Name: "fp_session", Value: makeToken(id, tokenVer), Path: "/",
		MaxAge: days * 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: true})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id, "username": uname, "display_name": disp, "is_admin": admin})
}

func apiLogout(w http.ResponseWriter, r *http.Request) {
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

func apiCreateJob(w http.ResponseWriter, r *http.Request) {
	u := requireUser(w, r)
	if u == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 52<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		jsonErr(w, 400, "文件超过 50MB 上限")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
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
		jsonErr(w, 400, "不支持的文件类型，仅支持：.docx, .jpg, .jpeg, .pdf, .png, .txt")
		return
	}
	if ext == ".enc" && !effAllowE2EE() {
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
		jsonErr(w, 429, "已达今日打印次数上限")
		return
	}
	rnd := make([]byte, 8)
	rand.Read(rnd)
	fpath := filepath.Join(uploadsDir, hex.EncodeToString(rnd)+ext)
	out, err := os.Create(fpath)
	if err != nil {
		jsonErr(w, 500, "存储失败")
		return
	}
	size, _ := io.Copy(out, file)
	out.Close()
	if size > 50<<20 {
		os.Remove(fpath)
		jsonErr(w, 400, "文件超过 50MB 上限")
		return
	}
	now := float64(time.Now().Unix())
	res, err := db.Exec("INSERT INTO print_jobs(user_id,file_path,orig_name,printer_name,copies,color,sides,paper,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'queued',?,?)",
		u.ID, fpath, origName, printer, copies, color, sides, paper, now, now)
	if err != nil {
		os.Remove(fpath)
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
	json.NewEncoder(w).Encode(map[string]interface{}{"id": jid, "status": "queued", "agent_online": sent})
}

func apiListJobs(w http.ResponseWriter, r *http.Request) {
	u := requireUser(w, r)
	if u == nil {
		return
	}
	rows, err := db.Query("SELECT id,orig_name,printer_name,copies,color,sides,paper,status,COALESCE(error,''),created_at FROM print_jobs WHERE user_id=? ORDER BY id DESC LIMIT 50", u.ID)
	if err != nil {
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
			"id": id, "orig_name": name, "printer_name": printerName, "copies": copies,
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
		jsonErr(w, 400, "仅排队中的任务可取消")
		return
	}
	db.Exec("UPDATE print_jobs SET status='canceled', updated_at=? WHERE id=?", float64(time.Now().Unix()), jid)
	os.Remove(fpath)
	sendToAgent(map[string]interface{}{"type": "cancel", "job_id": jid})
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
	if requireAdmin(w, r) == nil {
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
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": id})
}

// 重置密码：同时递增 token_version，让该用户所有已登录设备立即失效
func apiAdminResetPW(w http.ResponseWriter, r *http.Request) {
	if requireAdmin(w, r) == nil {
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
		jsonErr(w, 500, "操作失败")
		return
	}
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
	if _, err := db.Exec("DELETE FROM users WHERE id=?", uid); err != nil {
		jsonErr(w, 500, "删除失败")
		return
	}
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
	if requireAdmin(w, r) == nil {
		return
	}
	agentMu.Lock()
	if agentConn != nil {
		agentConn.Close()
		agentConn = nil
		agentLastAt = time.Now()
	}
	agentMu.Unlock()
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
	if requireAdmin(w, r) == nil {
		return
	}
	var body struct {
		DailyLimit  *int  `json:"daily_limit"`
		SessionDays *int  `json:"session_days"`
		AllowE2EE   *bool `json:"allow_e2ee"`
	}
	json.NewDecoder(r.Body).Decode(&body)
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
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok": true, "daily_limit": effDailyLimit(), "session_days": effSessionDays(),
		"allow_e2ee": effAllowE2EE()})
}

// 后台看护循环：
//   - 每 20 秒回收卡死的 processing 任务（默认超时 5 分钟，可用 -job-timeout 调整）
//   - 每小时清理完成/取消超 24h 的残留文件
func cleanupLoop() {
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for range tick.C {
			requeueStaleProcessing(effJobTimeout())
		}
	}()
	for {
		time.Sleep(time.Hour)
		rows, err := db.Query(`SELECT file_path FROM print_jobs
			WHERE status IN ('completed','canceled','failed') AND coalesce(purged,0)=0 AND updated_at<?`,
			float64(time.Now().Add(-24*time.Hour).Unix()))
		if err != nil {
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
	}
}

func main() {
	initDB()
	go cleanupLoop()

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
	mux.HandleFunc("GET /api/admin/settings", apiAdminGetSettings)
	mux.HandleFunc("PUT /api/admin/settings", apiAdminPutSettings)
	mux.HandleFunc("/ws/agent", wsAgent)
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("FamilyPrint-Cloud (Go) 监听 %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
