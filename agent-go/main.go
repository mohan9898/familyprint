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
	flag.Parse()
	if s := strings.TrimSpace(cloud); s != "" {
		cloudURLs = strings.Split(s, ",")
	}
	mock = mockStr == "1"
}

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
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed", "error": "文件解码失败"})
		return
	}
	printName, printData := msg.Filename, data
	if strings.HasSuffix(strings.ToLower(msg.Filename), ".enc") {
		// 是密文：必须解密后再打印。
		// 若家庭端没配密钥，绝不能把密文当明文送打印机（会打出一堆乱码纸，且误报成功）。
		if e2eeKey == "" {
			ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed",
				"error": "收到加密文件，但家庭端未配置家庭密钥，请用 -key 参数配置后重试"})
			log.Printf("任务 #%d 为加密文件，但未配置 -key，已拒绝打印", msg.JobID)
			return
		}
		name, dec, err := e2eeDecrypt(data)
		if err != nil {
			ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed",
				"error": "端到端解密失败（家庭密钥不匹配，请核对 -key 与网页端输入的密钥）"})
			return
		}
		printName, printData = name, dec
		log.Printf("任务 #%d 已解密: %s (%dKB)", msg.JobID, name, len(dec)/1024)
	}
	// 入队串行处理：先回 processing，避免用户在队列里看不到排队
	ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "processing"})
	select {
	case printQueue <- func() { runPrint(ws, msg, printName, printData) }:
	default:
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed",
			"error": "打印队列已满，请稍后重试"})
	}
}

// runPrint 在工作协程里串行执行真正的打印
func runPrint(ws *wsConn, msg jobMsg, printName string, printData []byte) {
	// 文件名来自用户上传，必须净化，防止 ../ 路径穿越覆盖系统文件
	safeName := filepath.Base(strings.ReplaceAll(printName, "\\", "/"))
	if safeName == "" || safeName == "." || safeName == "/" {
		safeName = fmt.Sprintf("job%d.pdf", msg.JobID)
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("fp_%d_%s", msg.JobID, safeName))
	if err := os.WriteFile(tmp, printData, 0600); err != nil {
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed", "error": "临时文件写入失败"})
		return
	}
	defer os.Remove(tmp) // 用完即删
	log.Printf("任务 #%d 开始打印: %s (%dKB)", msg.JobID, safeName, len(printData)/1024)
	if err := ippPrint(tmp, safeName, msg); err != nil {
		ws.send(map[string]interface{}{"type": "status", "job_id": msg.JobID, "status": "failed", "error": err.Error()})
	} else {
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
func reportPrinters(ws *wsConn) {
	status := "online"
	if !mock {
		httpURL := strings.Replace(printerURI, "ipp://", "http://", 1)
		req := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetPrinterAttributes, 1)
		req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("zh-CN")))
		req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(printerURI)))
		req.Operation.Add(goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("printer-state")))
		payload, encErr := req.EncodeBytes()
		if encErr != nil {
			status = "offline"
		} else {
			rsp, err := probeClient.Post(httpURL, goipp.ContentType, bytes.NewReader(payload))
			if err != nil {
				status = "offline"
			} else {
				rsp.Body.Close()
				// 只要打印机在 HTTP 层有响应（含 4xx/5xx 的 IPP 错误），就说明设备在线
				if rsp.StatusCode/100 == 5 {
					status = "offline"
				}
			}
		}
	}
	ws.send(map[string]interface{}{"type": "printers", "list": []map[string]string{{"name": printerName, "status": status}}})
}

func connectOnce(rawURL string, quit chan struct{}) error {
	if strings.HasPrefix(rawURL, "ws://") {
		log.Printf("⚠️ 警告: %s 使用未加密 ws:// 连接，生产环境应使用 wss://（域名+HTTPS）", rawURL)
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

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		return err
	}
	defer conn.Close()
	ws := &wsConn{conn: conn, mu: make(chan struct{}, 1)}
	log.Printf("已连接云端 %s", rawURL)
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
			return err
		}
		var m jobMsg
		json.Unmarshal(raw, &m)
		if m.Type == "job" {
			go handleJob(ws, raw)
		} else if m.Type == "cancel" {
			// 简化：家庭场景任务下发改打印后由打印机排队，取消仅对未下发任务有效（云端侧处理）
			log.Printf("任务 #%d 取消请求", m.JobID)
		}
	}
}

func main() {
	quit := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		close(quit)
		os.Exit(0)
	}()
	log.Printf("FamilyPrint-Agent 启动 | 云端: %v | 打印机: %s | MOCK=%v", cloudURLs, printerURI, mock)
	go printWorker() // 打印串行队列
	backoff := 2 * time.Second
	for {
		for _, u := range cloudURLs {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			err := connectOnce(u, quit)
			log.Printf("%s 连接断开: %v", u, err)
			select {
			case <-quit:
				return
			default:
			}
		}
		log.Printf("所有云端地址不可达，%v 后重试", backoff)
		time.Sleep(backoff)
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}
