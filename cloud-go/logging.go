// 日志基础设施：分级输出 + 请求 ID + 访问日志 + 审计日志（写数据库，后台可查）
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 级别 ----------
// 用 log.Printf 输出（带标准时间戳），级别前缀便于 grep / docker logs 过滤：
//
//	docker logs fp-cloud 2>&1 | grep ERROR
const (
	lvDebug = iota // 只有 LOG_LEVEL=debug 才输出（正常运行时噪音大）
	lvInfo
	lvWarn
	lvError
)

var logLevel = lvInfo

func initLogLevel() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		logLevel = lvDebug
	case "warn", "warning":
		logLevel = lvWarn
	case "error":
		logLevel = lvError
	default:
		logLevel = lvInfo
	}
}

// 级别开关（供内部判断，避免无谓的字符串拼接）
func debugOn() bool { return logLevel <= lvDebug }

var tagOf = [...]string{"DBG", "INF", "WRN", "ERR"}

// logf 统一出口：Tag 定宽 + 消息。故意不用 emoji，方便日志检索与机器解析。
func logf(tag int, format string, a ...interface{}) {
	if tag < logLevel {
		return
	}
	log.Printf("[%s] "+format, append([]interface{}{tagOf[tag]}, a...)...)
}

func logInfo(format string, a ...interface{})  { logf(lvInfo, format, a...) }
func logWarn(format string, a ...interface{})  { logf(lvWarn, format, a...) }
func logError(format string, a ...interface{}) { logf(lvError, format, a...) }
func logDebug(format string, a ...interface{}) { logf(lvDebug, format, a...) }

// ---------- 请求 ID ----------
// 一次请求（含它触发的多条日志）用同一个 8 位短 ID 串起来，
// 便于从"用户报障：03-05 14:22 打印失败"里捞出对应的全部日志行。
type ctxKey string

const ctxReqID ctxKey = "rid"

var reqSeq uint64
var reqSeqMu sync.Mutex

func newReqID() string {
	b := make([]byte, 4)
	rand.Read(b)
	reqSeqMu.Lock()
	reqSeq++
	n := reqSeq
	reqSeqMu.Unlock()
	return fmt.Sprintf("%s%04x", hex.EncodeToString(b), n&0xffff)
}

func ridOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxReqID).(string); ok {
		return v
	}
	return "-"
}

// logReq 带请求 ID 的日志，用于一次请求内的业务日志
func logReq(r *http.Request, tag int, format string, a ...interface{}) {
	logf(tag, "[%s] "+format, append([]interface{}{ridOf(r)}, a...)...)
}

// ---------- 来源 IP ----------
// 服务端通常在反代之后，r.RemoteAddr 是反代地址，真实来源在 X-Forwarded-For 首段
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// maskIP：日志里只保留网段前缀，避免把家人手机的公网 IP 完整落盘
func maskIP(ip string) string {
	if ip == "" {
		return "-"
	}
	if strings.HasPrefix(ip, "127.") || ip == "::1" || strings.HasPrefix(ip, "192.168.") ||
		strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "172.") {
		return ip // 内网地址无隐私顾虑，原样保留（排查方便）
	}
	parts := strings.Split(ip, ".")
	if len(parts) == 4 { // IPv4：1.2.x.x
		return parts[0] + "." + parts[1] + ".x.x"
	}
	if i := strings.Index(ip, ":"); i > 0 { // IPv6：前两段
		if seg := strings.Split(ip, ":"); len(seg) >= 2 {
			return seg[0] + ":" + seg[1] + ":*"
		}
	}
	return ip
}

// ---------- 访问日志 ----------
type statusWriter struct {
	http.ResponseWriter
	code int
	n    int64
}

func (w *statusWriter) WriteHeader(c int) {
	if w.code == 0 {
		w.code = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// Hijack：WebSocket 升级必须能拿到原始连接。
// 若包装器不实现 http.Hijacker，gorilla/websocket 会直接报
// "response does not implement http.Hijacker"，家庭端永远连不上。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("底层 ResponseWriter 不支持 Hijack，无法升级为 WebSocket")
	}
	if w.code == 0 {
		w.code = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

// Flush：静态文件/流式响应需要，避免包装后丢失
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withLog：全局中间件。
//   - 注入请求 ID
//   - 记录 方法/路径/状态码/耗时/体积/来源（静态资源只在 debug 级别记录，避免刷屏）
//   - 5xx / 4xx 用更高等级，方便一眼看出异常
func withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := newReqID()
		ctx := context.WithValue(r.Context(), ctxReqID, rid)
		r = r.WithContext(ctx)

		sw := &statusWriter{ResponseWriter: w}
		w.Header().Set("X-Request-Id", rid)
		next.ServeHTTP(sw, r)

		if sw.code == 0 {
			sw.code = 200
		}
		dur := time.Since(start)
		path := r.URL.Path
		static := !strings.HasPrefix(path, "/api/") && path != "/ws/agent"

		if static && sw.code < 400 {
			logDebug("[%s] %s %s %d %s %dB", rid, r.Method, path, sw.code, dur.Round(time.Millisecond), sw.n)
			return
		}
		tag := lvInfo
		if sw.code >= 500 {
			tag = lvError
		} else if sw.code >= 400 {
			tag = lvWarn
		}
		// 健康检查之类的高频路径只在慢请求时打印
		logf(tag, "[%s] %s %s %d %s %dB ip=%s", rid, r.Method, path, sw.code,
			dur.Round(time.Millisecond), sw.n, maskIP(clientIP(r)))
	})
}

// ---------- 审计日志（落库，管理后台可查）----------
// 家庭场景下管理员就是自己，但"谁把谁停用了""谁把每日上限从 30 改成 5"
// 这类变更事后一定要能追溯，否则出问题只能靠猜。
func initAuditTable() {
	db.Exec(`CREATE TABLE IF NOT EXISTS audit_log(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at REAL, actor_id INTEGER, actor TEXT,
		action TEXT, target TEXT, detail TEXT, ip TEXT)`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(created_at)`)
}

// audit 记录一条审计日志：同时打一行 INFO 到标准输出（docker logs 里能立刻看到）
func audit(r *http.Request, actorID int64, actor, action, target, detail string) {
	ip := maskIP(clientIP(r))
	db.Exec(`INSERT INTO audit_log(created_at,actor_id,actor,action,target,detail,ip) VALUES(?,?,?,?,?,?,?)`,
		float64(time.Now().Unix()), actorID, actor, action, target, detail, ip)
	if r != nil {
		if detail != "" {
			logReq(r, lvInfo, "审计: %s %s %s（%s）", actor, action, target, detail)
		} else {
			logReq(r, lvInfo, "审计: %s %s %s", actor, action, target)
		}
	} else if detail != "" {
		logInfo("审计: %s %s %s（%s）", actor, action, target, detail)
	} else {
		logInfo("审计: %s %s %s", actor, action, target)
	}
}

// 保留 90 天，每天清一次
func pruneAudit() {
	cut := float64(time.Now().Add(-90 * 24 * time.Hour).Unix())
	res, err := db.Exec("DELETE FROM audit_log WHERE created_at<?", cut)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			logInfo("清理过期审计日志 %d 条（保留 90 天）", n)
		}
	}
}

// ---------- 启动横幅 ----------
// 出问题时第一件事就是确认"跑的是哪个版本、什么配置"，启动时一次性打清楚。
func logStartupBanner() {
	var b strings.Builder
	line := func(k, v string) {
		b.WriteString(fmt.Sprintf("\n  %-14s %s", k, v))
	}
	b.WriteString("FamilyPrint-Cloud 启动")
	line("版本", buildVersion)
	line("监听", listenAddr)
	line("数据目录", dataDir)
	line("会话天数", strconv.Itoa(effSessionDays()))
	line("每日限额", strconv.Itoa(effDailyLimit())+" 个任务/人")
	line("任务超时", effJobTimeout().String())
	line("端到端加密", map[bool]string{true: "允许", false: "已关闭"}[effAllowE2EE()])
	line("设备token", maskSecret(deviceToken))
	line("日志级别", [...]string{"debug", "info", "warn", "error"}[logLevel])
	line("进程号", strconv.Itoa(os.Getpid()))

	var users, jobs, pending int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	db.QueryRow("SELECT COUNT(*) FROM print_jobs").Scan(&jobs)
	db.QueryRow("SELECT COUNT(*) FROM print_jobs WHERE status IN ('queued','processing')").Scan(&pending)
	line("账号/任务", fmt.Sprintf("%d 个账号，%d 条历史任务", users, jobs))
	if pending > 0 {
		line("待处理", fmt.Sprintf("%d 条（家庭端上线后会自动下发）", pending))
	}
	log.Print(b.String())
}

// maskSecret：日志里不打印完整密钥，只露首尾
func maskSecret(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", 8) + s[len(s)-4:]
}

// ---------- 启动时的一次性体检 ----------
func logSelfCheck() {
	// 1. 数据目录可写
	probe := dataDir + "/.write-probe"
	if err := os.WriteFile(probe, []byte("ok"), 0600); err != nil {
		logError("数据目录不可写: %v（备份/上传都会失败）", err)
	} else {
		os.Remove(probe)
		logDebug("数据目录可写: %s", dataDir)
	}
	// 2. 残留临时文件（异常退出可能留下）
	if ents, err := os.ReadDir(uploadsDir); err == nil {
		var orphans int
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			var n int
			db.QueryRow("SELECT COUNT(*) FROM print_jobs WHERE file_path LIKE ? AND COALESCE(purged,0)=0",
				"%"+e.Name()).Scan(&n)
			if n == 0 {
				orphans++
			}
		}
		if orphans > 0 {
			logWarn("uploads 目录有 %d 个无主文件（对应任务已清理），将在每小时清理时删除", orphans)
		}
	}
}

// 供管理接口按时间倒序取审计日志
type auditRow struct {
	ID     int64  `json:"id"`
	Time   string `json:"time"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	IP     string `json:"ip"`
}

func loadAudit(limit int) []auditRow {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.Query(`SELECT id, created_at, COALESCE(actor,''), COALESCE(action,''),
		COALESCE(target,''), COALESCE(detail,''), COALESCE(ip,'')
		FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return []auditRow{}
	}
	defer rows.Close()
	out := []auditRow{}
	for rows.Next() {
		var t float64
		var a auditRow
		rows.Scan(&a.ID, &t, &a.Actor, &a.Action, &a.Target, &a.Detail, &a.IP)
		a.Time = time.Unix(int64(t), 0).Format("01-02 15:04:05")
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
