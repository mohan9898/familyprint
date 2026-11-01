// FamilyPrint-Agent Go 版：WS 出站连接云端 → IPP 直连打印机 → 状态回报
// 内存 ~10MB，单二进制，无 CUPS 依赖（网络打印机 IPP 直连）
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/pbkdf2"
)

var (
	cloudURLs   = strings.Split(envOr("CLOUD_URLS", "ws://localhost:8443/ws/agent"), ",")
	deviceToken = envOr("DEVICE_TOKEN", "change-me-device-token")
	printerURI  = envOr("PRINTER_IPP_URL", "ipp://192.168.1.100:631/ipp/print")
	printerName = envOr("PRINTER_NAME", "家里打印机")
	e2eeKey     = os.Getenv("PRINT_KEY") // 端到端加密家庭密钥（与浏览器端一致）；为空则不启用
	mock        = envOr("MOCK", "0") == "1"
)

func init() {
	// 命令行参数（优先于环境变量），支持一条命令直接启动：
	// fp-agent -cloud wss://print.example.com/ws/agent -token xxx -printer ipp://192.168.1.100:631/ipp/print
	cloud := strings.Join(cloudURLs, ",")
	var mockStr string
	flag.StringVar(&cloud, "cloud", cloud, "云端地址，多个用逗号分隔（wss://host/ws/agent,...）")
	flag.StringVar(&deviceToken, "token", deviceToken, "设备 token（与云端一致）")
	flag.StringVar(&printerURI, "printer", printerURI, "打印机 IPP 地址")
	flag.StringVar(&printerName, "name", printerName, "打印机显示名")
	flag.StringVar(&e2eeKey, "key", e2eeKey, "端到端加密家庭密钥（可选，与浏览器端一致）")
	flag.StringVar(&mockStr, "mock", envOr("MOCK", "0"), "模拟打印（1=开启，调试用）")
	var dbgStr string
	flag.StringVar(&dbgStr, "debug", envOr("DEBUG", "0"), "调试模式（1=输出探活等详细日志）")
	flag.Parse()
	if s := strings.TrimSpace(cloud); s != "" {
		cloudURLs = strings.Split(s, ",")
	}
	mock = mockStr == "1"
	debug = dbgStr == "1"
}

// ---------- 日志：统一前缀 + 级别，便于 docker logs / journalctl 过滤 ----------
// 家庭端不需要复杂分级，只用 INF / WRN / ERR 三档，并统一加 [时间已由 log 包自带]
func logInf(format string, a ...interface{}) { log.Printf("[INF] "+format, a...) }
func logWrn(format string, a ...interface{}) { log.Printf("[WRN] "+format, a...) }
func logErr(format string, a ...interface{}) { log.Printf("[ERR] "+format, a...) }
func logDebug(format string, a ...interface{}) {
	if debug {
		log.Printf("[DBG] "+format, a...)
	}
}

// debug：DEBUG=1 或 -debug 时输出探活等心跳级日志（默认关，避免刷屏）
var debug = false

// 构建版本（CI 用 -ldflags 注入，未注入为 dev）
var buildVersion = "dev"

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// ippClient：必须设置超时。家用打印机断网/假死时，若用默认 client 会无限期挂起，
// 导致任务永远停在"打印中"、心跳 goroutine 被拖住。
var ippClient = &http.Client{Timeout: 120 * time.Second}

// probeClient：探活要快，避免拖慢心跳（30s 一次）。
var probeClient = &http.Client{Timeout: 4 * time.Second}

type wsConn struct {
	conn *websocket.Conn
	mu   chan struct{} // 串行化写
}

func (w *wsConn) send(v interface{}) error {
	w.mu <- struct{}{}
	defer func() { <-w.mu }()
	return w.conn.WriteJSON(v)
}

type jobMsg struct {
	Type     string `json:"type"`
	JobID    int    `json:"job_id"`
	Filename string `json:"filename"`
	Data     string `json:"data"`
	Params   struct {
		Copies int    `json:"copies"`
		Color  int    `json:"color"`
		Sides  string `json:"sides"`
		Paper  string `json:"paper"`
	} `json:"params"`
}

// fileSize: 取已打开文件的字节数（用于给 IPP 请求设置 Content-Length，
// 否则 httputil 会用 chunked 编码，部分打印机不接受）
func fileSize(f *os.File) int64 {
	if st, err := f.Stat(); err == nil {
		return st.Size()
	}
	return 0
}

// ippPrint: IPP Print-Job 直连打印机
func ippPrint(path, jobName string, p jobMsg) error {
	if mock {
		time.Sleep(15 * time.Second)
		return nil
	}
	mime := "application/octet-stream"
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".pdf":
		mime = "application/pdf"
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".png":
		mime = "image/png"
	case ".txt":
		mime = "text/plain"
	}
	// ipp:// → http://（IPP 就是 HTTP POST）
	httpURL := strings.Replace(printerURI, "ipp://", "http://", 1)

	req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpPrintJob, 1)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("zh-CN")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(printerURI)))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("familyprint")))
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String(jobName)))
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(mime)))
	// 作业模板参数
	req.Job.Add(goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(p.Params.Copies)))
	req.Job.Add(goipp.MakeAttribute("print-color-mode", goipp.TagKeyword,
		goipp.String(map[bool]string{true: "color", false: "monochrome"}[p.Params.Color == 1])))
	req.Job.Add(goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String(p.Params.Sides)))
	req.Job.Add(goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(p.Params.Paper)))

	payload, err := req.EncodeBytes()
	if err != nil {
		return fmt.Errorf("IPP 编码失败: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	httpReq, err := http.NewRequest(http.MethodPost, httpURL, io.MultiReader(bytes.NewReader(payload), f))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", goipp.ContentType)
	httpReq.Header.Set("Accept", goipp.ContentType)
	httpReq.ContentLength = int64(len(payload)) + fileSize(f)
	rsp, err := ippClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("打印机连接失败: %w", err)
	}
	defer rsp.Body.Close()
	if rsp.StatusCode/100 != 2 {
		return errors.New("打印机 HTTP " + rsp.Status)
	}
	m := &goipp.Message{}
	if err := m.Decode(rsp.Body); err != nil {
		return fmt.Errorf("IPP 响应解析失败: %w", err)
	}
	if goipp.Code(m.Code) != goipp.Code(goipp.StatusOk) &&
		goipp.Code(m.Code) != goipp.Code(goipp.StatusOkIgnoredSubscriptions) {
		return fmt.Errorf("打印机返回 IPP 状态: %v", m.Code)
	}
	return nil
}

// printQueue：家用打印机是独占设备，必须串行。并发向同一台打印机发 IPP，
// 会导致乱序、卡纸、甚至任务互相顶掉。
var printQueue = make(chan func(), 64)

func printWorker() {
	for task := range printQueue {
		task()
	}
}

func handleJob(ws *wsConn, raw []byte) {
	var msg jobMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Type != "job" {
		return
	}
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		logWrn("任务 #%d 数据解码失败: %v", msg.JobID, err)
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed", "error": "文件解码失败"})
		return
	}
	logInf("收到任务 #%d: %s (%dKB)", msg.JobID, msg.Filename, len(data)/1024)
	printName, printData := msg.Filename, data
	if strings.HasSuffix(strings.ToLower(msg.Filename), ".enc") {
		// 是密文：必须解密后再打印。
		// 若家庭端没配密钥，绝不能把密文当明文送打印机（会打出一堆乱码纸，且误报成功）。
		if e2eeKey == "" {
			ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed",
				"error": "收到加密文件，但家庭端未配置家庭密钥，请用 -key 参数配置后重试"})
			logErr("任务 #%d 是加密文件，但本机没配置 -key，已拒绝打印（否则会打出乱码）", msg.JobID)
			return
		}
		name, dec, err := e2eeDecrypt(data)
		if err != nil {
			logErr("任务 #%d 解密失败: %v（家庭密钥与网页端输入的不一致？）", msg.JobID, err)
			ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed",
				"error": "端到端解密失败（家庭密钥不匹配，请核对 -key 与网页端输入的密钥）"})
			return
		}
		printName, printData = name, dec
		logInf("任务 #%d 解密成功: %s (%dKB)", msg.JobID, name, len(dec)/1024)
	}
	// 入队串行处理：先回 processing，避免用户在队列里看不到排队
	ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "processing"})
	if waiting := len(printQueue); waiting > 0 {
		logInf("任务 #%d 进入打印队列，前面还有 %d 个任务在等", msg.JobID, waiting)
	}
	select {
	case printQueue <- func() { runPrint(ws, msg, printName, printData) }:
	default:
		logErr("任务 #%d 被拒: 打印队列已满（上限 %d）", msg.JobID, cap(printQueue))
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed",
			"error": "打印队列已满，请稍后重试"})
	}
}

// runPrint 在工作协程里串行执行真正的打印
func runPrint(ws *wsConn, msg jobMsg, printName string, printData []byte) {
	start := time.Now()
	// 文件名来自用户上传，必须净化，防止 ../ 路径穿越覆盖系统文件
	safeName := filepath.Base(strings.ReplaceAll(printName, "\\", "/"))
	if safeName == "" || safeName == "." || safeName == "/" {
		safeName = fmt.Sprintf("job%d.pdf", msg.JobID)
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("fp_%d_%s", msg.JobID, safeName))
	if err := os.WriteFile(tmp, printData, 0600); err != nil {
		logErr("任务 #%d 临时文件写入失败: %v（磁盘满 / /tmp 不可写？）", msg.JobID, err)
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed", "error": "临时文件写入失败"})
		return
	}
	defer os.Remove(tmp) // 用完即删
	logInf("任务 #%d 开始打印: %s (%dKB, %d份, %s, %s)", msg.JobID, safeName, len(printData)/1024,
		msg.Params.Copies, map[bool]string{true: "彩色", false: "黑白"}[msg.Params.Color == 1],
		map[bool]string{true: "双面", false: "单面"}[msg.Params.Sides == "two-sided"])
	err := ippPrint(tmp, safeName, msg)
	took := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		logErr("任务 #%d 打印失败（耗时 %s）: %v", msg.JobID, took, err)
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed", "error": err.Error()})
	} else {
		logInf("任务 #%d 打印完成（耗时 %s）", msg.JobID, took)
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "completed"})
	}
}

// ---------- 端到端加密（与浏览器 WebCrypto 对齐）----------
// 密文格式: base64(iv12B) + "." + base64(AES-256-GCM( JSON{n,m,d} ))
// 密钥: PBKDF2-HMAC-SHA256(口令, "familyprint-e2ee-v1", 100000, 32)
func e2eeDecrypt(blob []byte) (string, []byte, error) {
	parts := strings.SplitN(string(blob), ".", 2)
	if len(parts) != 2 {
		return "", nil, errors.New("密文格式错误")
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", nil, err
	}
	key := pbkdf2.Key([]byte(e2eeKey), []byte("familyprint-e2ee-v1"), 100000, 32, sha256.New)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", nil, err
	}
	pt, err := gcm.Open(nil, iv, ct, nil)
	if err != nil {
		return "", nil, err
	}
	var inner struct {
		N string `json:"n"`
		M string `json:"m"`
		D string `json:"d"`
	}
	if err := json.Unmarshal(pt, &inner); err != nil {
		return "", nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(inner.D)
	if err != nil {
		return "", nil, err
	}
	return inner.N, raw, nil
}

// reportPrinters：探活打印机并上报状态。
// 用 Get-Printer-Attributes 更准确；部分老打印机不支持，退化为 TCP 连通性判断。
// 只在"状态发生变化"时打日志，避免每 30 秒刷一条。
var lastPrinterStatus = ""

func reportPrinters(ws *wsConn) {
	start := time.Now()
	status := "online"
	reason := ""
	if !mock {
		httpURL := strings.Replace(printerURI, "ipp://", "http://", 1)
		req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
		req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("zh-CN")))
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(printerURI)))
		req.Operation.Add(goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("printer-state")))
		payload, encErr := req.EncodeBytes()
		if encErr != nil {
			status, reason = "offline", "IPP 请求编码失败: "+encErr.Error()
		} else {
			rsp, err := probeClient.Post(httpURL, goipp.ContentType, bytes.NewReader(payload))
			if err != nil {
				status, reason = "offline", "探测失败: "+err.Error()
			} else {
				rsp.Body.Close()
				// 只要打印机在 HTTP 层有响应（含 4xx/5xx 的 IPP 错误），就说明设备在线
				if rsp.StatusCode/100 == 5 {
					status, reason = "offline", "打印机返回 "+rsp.Status
				}
			}
		}
	}
	if status != lastPrinterStatus {
		switch status {
		case "online":
			if lastPrinterStatus == "" {
				logInf("打印机「%s」在线，探测耗时 %s", printerName, time.Since(start).Round(time.Millisecond))
			} else {
				logInf("打印机「%s」恢复在线", printerName)
			}
		case "offline":
			logWrn("打印机「%s」离线：%s（检查电源/WiFi、IPP 地址 %s 是否正确、IP 是否变化）",
				printerName, reason, printerURI)
		}
		lastPrinterStatus = status
	} else if debug {
		logDebug("探活 %s → %s（%s）", printerURI, status, time.Since(start).Round(time.Millisecond))
	}
	ws.send(map[string]interface{}{"type": "printers", "list": []map[string]string{{"name": printerName, "status": status}}})
}

// errNeverConnected：本次连接在"成功建立"之前就失败了，用于区分「配置错」与「正常断线重连」
var errNeverConnected = errors.New("连接建立失败")

func connectOnce(rawURL string, quit chan struct{}) error {
	if strings.HasPrefix(rawURL, "ws://") {
		logWrn("%s 使用未加密 ws:// 连接，生产环境应使用 wss://（域名+HTTPS）", rawURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("token", deviceToken)
	u.RawQuery = q.Encode()
	header := http.Header{}
	header.Set("X-Device-Token", deviceToken)

	conn, rsp, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		// 403 几乎一定是两端 token 不一致；这里给出明确指向，省得用户翻文档
		if rsp != nil && rsp.StatusCode == 403 {
			return fmt.Errorf("%w: 云端拒绝了设备 token（HTTP 403），请核对两端 DEVICE_TOKEN 是否完全一致", errNeverConnected)
		}
		return fmt.Errorf("%w: %v", errNeverConnected, err)
	}
	defer conn.Close()
	ws := &wsConn{conn: conn, mu: make(chan struct{}, 1)}
	connectedAt := time.Now()
	logInf("已连接云端 %s", rawURL)
	reportPrinters(ws)

	// 心跳
	done := make(chan struct{})
	var doneOnce sync.Once
	stop := func() { doneOnce.Do(func() { close(done) }) }
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				deadline := time.Now().Add(10 * time.Second)
				if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
					conn.Close()
					stop()
					return
				}
				reportPrinters(ws)
			case <-done:
				return
			}
		}
	}()
	defer stop() // 无论从哪条路径退出，都保证只关一次

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			uptime := time.Since(connectedAt).Round(time.Second)
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return fmt.Errorf("连接异常断开（已保持 %s）: %w", uptime, err)
			}
			return fmt.Errorf("连接已被关闭（已保持 %s）", uptime)
		}
		var m jobMsg
		json.Unmarshal(raw, &m)
		if m.Type == "job" {
			go handleJob(ws, raw)
		} else if m.Type == "cancel" {
			// 简化：家庭场景任务下发改打印后由打印机排队，取消仅对未下发任务有效（云端侧处理）
			logInf("收到任务 #%d 取消通知（若已进打印机队列，可能仍会出纸）", m.JobID)
		} else {
			logDebug("收到未识别的消息类型 %q，已忽略", m.Type)
		}
	}
}

func main() {
	quit := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logInf("收到退出信号，正在停止…")
		close(quit)
		os.Exit(0)
	}()
	// 启动横幅：排查问题时先确认"跑的是哪个版本、连的是哪、打印机地址对不对"
	logInf("FamilyPrint-Agent 启动")
	logInf("  版本      %s", buildVersion)
	logInf("  云端      %v", cloudURLs)
	logInf("  打印机    %s（%s）", printerName, printerURI)
	logInf("  端到端    %s", map[bool]string{true: "已启用（-key 已配置）", false: "未启用"}[e2eeKey != ""])
	logInf("  模拟模式  %v", mock)
	logInf("  调试日志  %v", debug)
	go printWorker() // 打印串行队列

	backoff := 2 * time.Second
	fails := 0
	for {
		for _, u := range cloudURLs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			err := connectOnce(u, quit)
			select {
			case <-quit:
				return
			default:
			}
			if errors.Is(err, errNeverConnected) {
				// 一次都没连上：多半是域名/token/网络问题，短间隔快速重试
				fails++
				if fails <= 3 || debug {
					logErr("%s 连不上（第 %d 次重试）: %v", u, fails, err)
					if fails == 3 {
						logWrn("连续失败 3 次仍在重试，之后不再逐条打印连接错误（用 -debug 1 可看全部）")
					}
				}
			} else {
				// 连上过又断开：说明配置没问题，正常重连即可
				logWrn("%s 连接断开: %v", u, err)
				fails = 0
			}
		}
		select {
		case <-quit:
			return
		default:
		}
		logInf("%.0f 秒后重连（累计失败 %d 次）", backoff.Seconds(), fails)
		time.Sleep(backoff)
		if fails > 0 && backoff < 60*time.Second {
			backoff *= 2
		} else {
			backoff = 2 * time.Second
		}
	}
}
