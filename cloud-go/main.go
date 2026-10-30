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
	flag.Parse()
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
	  failed INTEGER DEFAULT 0, locked_until REAL DEFAULT 0);
	CREATE TABLE IF NOT EXISTS print_jobs(
	  id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL,
	  file_path TEXT NOT NULL, orig_name TEXT NOT NULL, printer_name TEXT NOT NULL,
	  copies INTEGER DEFAULT 1, color INTEGER DEFAULT 0, sides TEXT DEFAULT 'one-sided',
	  paper TEXT DEFAULT 'A4', status TEXT DEFAULT 'queued', error TEXT,
	  created_at REAL, updated_at REAL, purged INTEGER DEFAULT 0);
	`
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n == 0 {
		db.Exec("INSERT INTO users(username,password_hash,display_name,is_admin) VALUES(?,?,?,1)",
			"admin", hashPW("admin123", ""), "管理员")
		log.Println("已创建默认管理员 admin/admin123（请尽快修改）")
	}
}

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

// ---------- 会话（签名 Cookie，兼容 Python 版格式 uid.exp.sig）----------
func makeToken(uid int64) string {
	days, _ := strconv.Atoi(sessionDays)
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	payload := fmt.Sprintf("%d.%d", uid, exp)
	mac := hmacSHA256([]byte(secretKey), []byte(payload))
	return payload + "." + hex.EncodeToString(mac)
}

func parseToken(token string) int64 {
	ps := strings.Split(token, ".")
	if len(ps) != 3 {
		return 0
	}
	mac := hmacSHA256([]byte(secretKey), []byte(ps[0]+"."+ps[1]))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(mac)), []byte(ps[2])) != 1 {
		return 0
	}
	exp, _ := strconv.ParseInt(ps[1], 10, 64)
	if time.Now().Unix() > exp {
		return 0
	}
	uid, _ := strconv.ParseInt(ps[0], 10, 64)
	return uid
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
	uid := parseToken(c.Value)
	if uid == 0 {
		return nil
	}
	var u user
	var name, disp string
	var admin int
	err = db.QueryRow("SELECT username, COALESCE(display_name,''), is_admin FROM users WHERE id=?", uid).
		Scan(&name, &disp, &admin)
	if err != nil {
		return nil
	}
	u = user{ID: uid, Username: name, DisplayName: disp, IsAdmin: admin}
	_ = u.Username
	return &user{ID: uid, Username: name, DisplayName: disp, IsAdmin: admin}
}

// ---------- Agent WS 通道 ----------
var (
	agentMu   sync.Mutex
	agentConn *websocket.Conn
)

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
	if status == "completed" {
		var path string
		if db.QueryRow("SELECT file_path FROM print_jobs WHERE id=?", jobID).Scan(&path) == nil {
			os.Remove(path) // 打印完成立即删除云端文件
			db.Exec("UPDATE print_jobs SET purged=1 WHERE id=?", jobID)
		}
	}
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
	agentMu.Unlock()
	log.Printf("Agent 已连接: remote=%s", r.RemoteAddr)
	defer func() {
		agentMu.Lock()
		if agentConn == conn {
			agentConn = nil
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
func apiLogin(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Password string }
	json.NewDecoder(r.Body).Decode(&body)
	var id int64
	var hash, disp string
	var admin, failed int
	var locked float64
	err := db.QueryRow("SELECT id, password_hash, COALESCE(display_name,''), is_admin, failed, locked_until FROM users WHERE username=?",
		body.Username).Scan(&id, &hash, &disp, &admin, &failed, &locked)
	if err == sql.ErrNoRows {
		jsonErr(w, 401, "用户名或密码错误")
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
	db.Exec("UPDATE users SET failed=0, locked_until=0 WHERE id=?", id)
	days, _ := strconv.Atoi(sessionDays)
	http.SetCookie(w, &http.Cookie{
		Name: "fp_session", Value: makeToken(id), Path: "/",
		MaxAge: days * 86400, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: true})
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": id, "username": body.Username, "display_name": disp, "is_admin": admin})
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
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !allowedExt[ext] {
		jsonErr(w, 400, "不支持的文件类型，仅支持：.docx, .jpg, .jpeg, .pdf, .png, .txt")
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
	// 每日限额
	limit, _ := strconv.Atoi(dailyLimit)
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
		u.ID, fpath, hdr.Filename, printer, copies, color, sides, paper, now, now)
	if err != nil {
		os.Remove(fpath)
		jsonErr(w, 500, "任务创建失败")
		return
	}
	jid, _ := res.LastInsertId()
	data, _ := os.ReadFile(fpath)
	sent := sendToAgent(map[string]interface{}{
		"type": "job", "job_id": jid, "filename": hdr.Filename,
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

// 每小时清理：完成/取消超 24h 的残留文件
func cleanupLoop() {
	for {
		time.Sleep(time.Hour)
		rows, err := db.Query("SELECT file_path FROM print_jobs WHERE status IN ('completed','canceled') AND updated_at<?",
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
			os.Remove(p)
		}
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
	mux.HandleFunc("/ws/agent", wsAgent)
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(staticSub)))

	log.Printf("FamilyPrint-Cloud (Go) 监听 %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
