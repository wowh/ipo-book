// Package httpapi 把本地账本暴露成只监听本机的 HTTP 接口，供 AI agent 或脚本读写。
//
// 桌面应用本身是双击打开的窗口，agent 没法点界面，所以这里另开一个回环地址上的
// JSON 接口：地址与令牌写在数据目录的 api.json 里，同机进程读到这个文件就能接入，
// 外部机器连不上，账本也不用放到公网上。
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wowh/ipo-book/internal/api"
)

const (
	// DefaultAddr 是默认监听地址。只绑定回环网络，不接受外部连接。
	DefaultAddr = "127.0.0.1:47123"
	// DiscoveryName 是数据目录下记录接口地址与令牌的文件名。
	DiscoveryName = "api.json"
	// tokenFileName 是令牌文件名。
	tokenFileName = "api-token"
	// pointerDirName 是家目录下存放接入指针的目录名。
	pointerDirName = ".ipo-book"
)

// SkillDirName 是默认的 skill 导出目录，相对数据目录。
var SkillDirName = filepath.Join("skill", "ipo-book")

// PointerPath 返回跨平台固定位置的接入指针：~/.ipo-book/api.json。
//
// 接口端口可能被占用而退到随机端口，数据目录也可能被 -data 改到别处，
// agent 无法预先知道端口。指针文件放在用户主目录下的固定位置，内容与数据
// 目录里的发现文件一致，另带一个 discovery 字段指向真正的发现文件。
func PointerPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("无法确定用户主目录")
	}
	return filepath.Join(home, pointerDirName, DiscoveryName), nil
}

// ErrNotRunning 表示接口服务没有启动。
var ErrNotRunning = errors.New("本地接口未启动")

// Server 是本机 HTTP 接口。
type Server struct {
	svc     *api.Service
	dataDir string
	version string
	appName string

	mu      sync.Mutex
	token   string
	baseURL string
	ln      net.Listener
	srv     *http.Server
	started time.Time
	// pointer 表示是否同时写家目录下的接入指针。只有常驻的应用实例该写：
	// 临时的 -export-skill 进程写会覆盖别人的指针，退出时又把它删掉。
	pointer bool
	// docs 是接口自述，启动时按注册的路由生成，GET /api 直接返回它。
	docs []endpointDoc
}

// New 创建接口服务。dataDir 用于存放令牌、发现文件与默认的 skill 目录。
func New(svc *api.Service, dataDir, appName, version string) *Server {
	return &Server{svc: svc, dataDir: dataDir, appName: appName, version: version}
}

// EnablePointer 让服务在启动时同时写家目录下的接入指针。
func (s *Server) EnablePointer() { s.pointer = true }

// Start 载入（必要时新建）令牌并开始监听。
//
// addr 传空时用 DefaultAddr；端口被占用时应由调用方改用 "127.0.0.1:0" 重试，
// 而不是让整个应用起不来。
func (s *Server) Start(addr string) error {
	if addr == "" {
		addr = DefaultAddr
	}
	token, err := loadOrCreateToken(filepath.Join(s.dataDir, tokenFileName))
	if err != nil {
		return fmt.Errorf("准备访问令牌失败：%w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败：%w", addr, err)
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}

	s.mu.Lock()
	s.token = token
	s.ln = ln
	s.srv = srv
	s.baseURL = "http://" + ln.Addr().String()
	s.started = time.Now()
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("本地接口停止：%v", err)
		}
	}()
	if err := s.writeDiscovery(); err != nil {
		// 发现文件写不出来，agent 就找不到地址，等同于没启动。
		s.Close()
		return err
	}
	log.Printf("本地接口已启动：%s", s.BaseURL())
	return nil
}

// Close 停止监听并删掉发现文件。删掉是有意的：留着一个指向死端口的文件，
// agent 只会拿到连接失败的报错，不如让它看到「文件不存在」。
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	s.srv, s.ln, s.baseURL = nil, nil, ""
	pointer := s.pointer
	s.mu.Unlock()
	_ = os.Remove(filepath.Join(s.dataDir, DiscoveryName))
	if pointer {
		removePointerOf(os.Getpid())
	}
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// removePointerOf 只删掉属于本进程的指针文件。
//
// 万一同时有别的实例（比如换了数据目录）写过指针，删掉它就等于把别人的接入
// 信息抹了，所以先核对里面的进程号。
func removePointerOf(pid int) {
	path, err := PointerPath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var found struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(data, &found) == nil && found.PID != 0 && found.PID != pid {
		return
	}
	_ = os.Remove(path)
}

// BaseURL 返回接口根地址，未启动时为空。
func (s *Server) BaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseURL
}

// Token 返回当前访问令牌。
func (s *Server) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// RotateToken 作废旧令牌并换一个新的，用于怀疑令牌外泄时。
func (s *Server) RotateToken() (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := writeToken(filepath.Join(s.dataDir, tokenFileName), token); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.token = token
	running := s.srv != nil
	s.mu.Unlock()
	if running {
		if err := s.writeDiscovery(); err != nil {
			return token, err
		}
	}
	return token, nil
}

// DiscoveryPath 返回发现文件的路径。
func (s *Server) DiscoveryPath() string {
	return filepath.Join(s.dataDir, DiscoveryName)
}

// DataPath 返回账本数据库路径。
func (s *Server) DataPath() string { return s.svc.StorePath() }

// SkillDir 返回默认的 skill 导出目录。
func (s *Server) SkillDir() string {
	return filepath.Join(s.dataDir, SkillDirName)
}

// Info 是给界面展示的接入信息。
type Info struct {
	// Running 表示接口正在监听。
	Running bool `json:"running"`
	// BaseURL 是接口根地址。
	BaseURL string `json:"baseUrl"`
	// Token 是访问令牌。
	Token string `json:"token"`
	// DiscoveryPath 是发现文件路径。
	DiscoveryPath string `json:"discoveryPath"`
	// PointerPath 是家目录下的固定指针，agent 从这里找接口。
	PointerPath string `json:"pointerPath"`
	// DataPath 是账本数据库路径。
	DataPath string `json:"dataPath"`
	// SkillDir 是默认 skill 目录。
	SkillDir string `json:"skillDir"`
	// SkillExported 表示默认目录下已经有 skill 包。
	SkillExported bool `json:"skillExported"`
	// Addr 是监听地址，未启动时为空。
	Addr string `json:"addr"`
}

// Info 读取接入信息。
func (s *Server) Info() Info {
	s.mu.Lock()
	baseURL, token, ln, running := s.baseURL, s.token, s.ln, s.srv != nil
	s.mu.Unlock()

	info := Info{
		Running:       running,
		BaseURL:       baseURL,
		Token:         token,
		DiscoveryPath: s.DiscoveryPath(),
		DataPath:      s.DataPath(),
		SkillDir:      s.SkillDir(),
	}
	if path, err := PointerPath(); err == nil {
		info.PointerPath = path
	}
	if ln != nil {
		info.Addr = ln.Addr().String()
	}
	if _, err := os.Stat(filepath.Join(info.SkillDir, "SKILL.md")); err == nil {
		info.SkillExported = true
	}
	return info
}

// discovery 是 api.json 的内容，agent 读这一个文件就够了。
type discovery struct {
	App       string `json:"app"`
	Version   string `json:"version"`
	BaseURL   string `json:"baseUrl"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	DataPath  string `json:"dataPath"`
	SkillDir  string `json:"skillDir"`
	// Discovery 只在指针文件里出现，指向真正的发现文件。
	Discovery string `json:"discovery,omitempty"`
	Hint      string `json:"hint"`
}

// discoveryPayload 生成发现文件的内容。realDiscovery 非空表示这是指针文件。
func (s *Server) discoveryPayload(realDiscovery string) ([]byte, error) {
	s.mu.Lock()
	baseURL, token, started := s.baseURL, s.token, s.started
	s.mu.Unlock()
	if baseURL == "" {
		return nil, ErrNotRunning
	}
	return json.MarshalIndent(discovery{
		App:       s.appName,
		Version:   s.version,
		BaseURL:   baseURL,
		Token:     token,
		PID:       os.Getpid(),
		StartedAt: started.Format(time.RFC3339),
		DataPath:  s.DataPath(),
		SkillDir:  s.SkillDir(),
		Discovery: realDiscovery,
		Hint:      "请求带上 Authorization: Bearer <token>；接口只在本机可访问。",
	}, "", "  ")
}

// writeDiscovery 把接入信息写到数据目录，必要时再写一份家目录下的指针。
//
// 文件权限 0600：令牌等同于账本的写权限，不能让同机其他用户读走。
func (s *Server) writeDiscovery() error {
	payload, err := s.discoveryPayload("")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	path := s.DiscoveryPath()
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", filepath.Base(path), err)
	}
	if s.pointer {
		s.writePointer()
	}
	return nil
}

// writePointer 在家目录下写一份固定位置的接入指针。
//
// 写不出来不影响接口本身：数据目录里的发现文件仍是权威来源，agent 还有那一条
// 路径可走，所以这里只记日志，不中断启动。
func (s *Server) writePointer() {
	path, err := PointerPath()
	if err != nil {
		log.Printf("无法确定接入指针位置：%v", err)
		return
	}
	payload, err := s.discoveryPayload(s.DiscoveryPath())
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("无法创建 %s：%v", filepath.Dir(path), err)
		return
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		log.Printf("写入接入指针失败：%v", err)
	}
}

// newToken 生成 32 字节的随机令牌。
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// loadOrCreateToken 读取令牌，第一次运行时创建。
func loadOrCreateToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); len(token) >= 16 {
			return token, nil
		}
		// 内容坏了就换新的，否则接口永远起不来。
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := writeToken(path, token); err != nil {
		return "", err
	}
	return token, nil
}

// writeToken 覆盖写入令牌文件。
func writeToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入令牌失败：%w", err)
	}
	return nil
}

// guard 校验来源与令牌，并记录会改数据的请求。
//
// 三道关：带 Origin 的请求一律拒绝（浏览器发起的跨站请求都带），Host 必须是
// 本机（防 DNS rebinding），然后是令牌。窗口自身走的是应用内部通道，不经过
// 这里，所以拒绝网页请求不会影响界面。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			writeError(w, http.StatusForbidden, "forbidden", "本地接口不接受浏览器跨站请求")
			return
		}
		if !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden", "本地接口只接受本机地址访问")
			return
		}
		if !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ipo-book"`)
			writeError(w, http.StatusUnauthorized, "unauthorized",
				"缺少或错误的访问令牌，令牌写在数据目录的 "+DiscoveryName+" 里")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// 改数据的操作留一条日志，出问题时能对上是谁改的。
			log.Printf("接口 %s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// authorized 判断请求是否带了正确令牌。
func (s *Server) authorized(r *http.Request) bool {
	token := s.Token()
	if token == "" {
		return false
	}
	header := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(header, "Bearer "); ok {
		header = after
	}
	if subtleEqual(strings.TrimSpace(header), token) {
		return true
	}
	return subtleEqual(strings.TrimSpace(r.Header.Get("X-API-Token")), token)
}

// subtleEqual 定长时间比较令牌，避免按前缀逐位试探。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// isLoopbackHost 判断 Host 头是否指向本机。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// routes 注册全部接口。
//
// 路由表同时用于注册与生成接口清单：GET /api 返回的自述就是这张表，写文档时
// 不会和实际路由对不上。
func (s *Server) routes() http.Handler {
	table := []struct {
		method  string
		path    string
		summary string
		handle  http.HandlerFunc
	}{
		{"GET", "/api", "接口清单与版本信息", s.handleIndex},
		{"GET", "/api/health", "健康检查：能否连上、有多少数据", s.handleHealth},
		{"GET", "/api/snapshot", "一次取回新股、券商账户、记录与汇总", s.handleSnapshot},
		{"GET", "/api/summary", "收益汇总", s.handleSummary},
		{"GET", "/api/stats", "收益统计，支持 year、month、brokerId 筛选，附按年、按月拆分", s.handleStats},

		{"GET", "/api/issues", "新股列表，支持 scope=all|issuing|done、q=关键词、limit", s.handleIssues},
		{"POST", "/api/issues/refresh", "从北交所同步新股列表，body 可给 {scope}", s.handleRefresh},

		{"GET", "/api/brokers", "券商账户列表", s.handleBrokers},
		{"POST", "/api/brokers", "新增券商账户", s.handleSaveBroker},
		{"PATCH", "/api/brokers/{id}", "局部修改券商账户", s.handlePatchBroker},
		{"DELETE", "/api/brokers/{id}", "删除券商账户，账户下有记录时拒绝", s.handleDeleteBroker},

		{"GET", "/api/entries", "打新记录列表，支持 fxCode、brokerId、status 过滤", s.handleEntries},
		{"POST", "/api/entries", "新增一条打新记录，卖出用 sales 数组", s.handleSaveEntry},
		{"GET", "/api/entries/{id}", "按 id 取一条记录，含卖出明细", s.handleEntry},
		{"PATCH", "/api/entries/{id}", "局部修改一条记录", s.handlePatchEntry},
		{"DELETE", "/api/entries/{id}", "删除一条记录", s.handleDeleteEntry},
		{"POST", "/api/entries/{id}/sales", "追加一笔卖出，不必回写整条记录", s.handleAppendSale},
		{"PATCH", "/api/entries/{id}/sales/{saleId}", "整笔修改某一笔卖出", s.handlePatchSale},
		{"DELETE", "/api/entries/{id}/sales/{saleId}", "删除某一笔卖出", s.handleDeleteSale},

		{"GET", "/api/backup", "导出账本备份（JSON）", s.handleBackup},
		{"POST", "/api/backup/restore", "用备份整体替换账本，须带 confirm=replace", s.handleRestore},
		{"GET", "/api/skill", "skill 包的位置与文件清单", s.handleSkillInfo},
		{"POST", "/api/skill/export", "导出 skill 包，body 可给 {dir}", s.handleSkillExport},
	}

	mux := http.NewServeMux()
	docs := make([]endpointDoc, 0, len(table))
	for _, e := range table {
		mux.HandleFunc(e.method+" "+e.path, e.handle)
		docs = append(docs, endpointDoc{Method: e.method, Path: e.path, Summary: e.summary})
	}
	mux.HandleFunc("/", s.handleNotFound)

	s.mu.Lock()
	s.docs = docs
	s.mu.Unlock()
	return s.guard(mux)
}
