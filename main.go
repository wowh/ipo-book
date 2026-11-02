// Command bse-ipo-book 是一个在本地运行的北交所打新账本。
//
// 程序用 MyGo 提供一个跨平台的桌面窗口，界面是打包进二进制的网页；后端逻辑与
// 数据存储全部在这台机器上完成，不依赖服务器。新股列表来自北交所公开的发行
// 信息，主源不可用时自动切换到备用源。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/egoist/mygo"

	"github.com/wowh/ipo-book/internal/api"
	"github.com/wowh/ipo-book/internal/httpapi"
	"github.com/wowh/ipo-book/internal/source"
	"github.com/wowh/ipo-book/internal/store"
	"github.com/wowh/ipo-book/web"
)

// appName 是数据目录名，也是备份文件名的前缀。
const appName = "bse-ipo-book"

// appTitle 是窗口标题，也是本地接口自述里的应用名。
const appTitle = "打新账本"

// version 由构建脚本通过 -ldflags 注入；用 mygo build 打包时留空，
// 版本号改从应用配置读取。
var version = ""

// mainWindow 是唯一的窗口，第二次启动时用来把已有窗口提到前台。
var mainWindow *mygo.Window

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		// 双击启动时参数由系统给，解析失败不该让窗口起不来。
		fmt.Fprintln(os.Stderr, err)
	}
	if opts.showVersion {
		fmt.Println(appName, displayVersion())
		return
	}
	if opts.exportSkill != "" {
		// 导出 skill 不需要窗口，装完就能先把说明交给 agent。
		if err := exportSkillOnly(opts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// 已经有一个实例在跑时，把它的窗口提到前台，而不是再开一个。
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}

	dir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		mygo.App.WhenReady(func() { showFatal("无法确定数据目录", err) })
		_ = mygo.App.Run()
		return
	}
	if err := setupLog(dir); err != nil {
		// 日志写不了也继续，只是排错信息少一些。
		fmt.Fprintf(os.Stderr, "创建日志文件失败：%v\n", err)
	}
	defer closeLog()

	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		mygo.App.WhenReady(func() { showFatal("打开账本失败", err) })
		_ = mygo.App.Run()
		return
	}
	defer st.Close()

	svc := api.New(st, source.NewManager(source.NewBSEPrimary(), source.NewBSEBackup()))

	// 本地接口让 agent 与脚本也能读写账本，界面本身不经过它。
	var apiServer *httpapi.Server
	if !opts.noAPI {
		apiServer = startAPI(svc, dir, opts.apiAddr)
		if apiServer != nil {
			defer apiServer.Close()
		}
	}

	// 前端文件与 Go 服务一起编译进二进制，运行时不依赖外部文件。
	mygo.SetFrontend(web.Files)
	mygo.Bind(svc)
	// 服务名默认取类型名，desktop 是小写类型，显式给出页面调用的名字。
	mygo.BindAs("Desktop", &desktop{service: svc, api: apiServer})

	mygo.App.OnSecondInstance(func([]string, string) {
		if mainWindow != nil {
			mainWindow.Focus()
		}
		mygo.App.Focus()
	})
	mygo.App.SetMenu(appMenu())

	mygo.App.WhenReady(func() {
		// 截图时允许把窗口拉高，好把抽屉里较长的卖出明细一次拍全。
		height := 800
		if opts.captureHeight > height {
			height = opts.captureHeight
		}
		// 宽度也可以指定，用来检查最窄窗口下列表会不会挤。
		width := 1180
		if opts.captureWidth > 0 {
			width = opts.captureWidth
		}
		mainWindow = mygo.NewWindow(mygo.WindowOptions{
			Title:     appTitle,
			URL:       startURL(opts.captureView),
			Width:     width,
			Height:    height,
			MinWidth:  1040,
			MinHeight: 620,
		})
		log.Printf("窗口已创建，等待界面加载")
		if opts.capturePath != "" {
			go captureAndQuit(mainWindow, opts.capturePath)
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Printf("启动窗口失败：%v", err)
		os.Exit(1)
	}
}

// options 是命令行参数。
type options struct {
	// dataDir 是账本数据目录，空表示用系统用户数据目录。
	dataDir string
	// showVersion 表示打印版本号后退出。
	showVersion bool
	// capturePath 是开发用的界面截图路径，非空时界面加载后截图并退出。
	capturePath string
	// captureView 是截图前要切换到的标签页，如 entries。
	captureView string
	// captureHeight 是截图时的窗口高度，0 表示用默认高度。
	captureHeight int
	// captureWidth 是截图时的窗口宽度，0 表示用默认宽度。
	captureWidth int
	// apiAddr 是本地接口的监听地址。
	apiAddr string
	// noAPI 表示不启动本地接口。
	noAPI bool
	// exportSkill 非空时只导出 skill 包到该目录后退出。
	exportSkill string
}

// parseFlags 解析命令行参数，忽略系统或启动器附带的未知参数。
//
// 从访达、启动台或 Dock 启动 macOS 应用时，系统会在参数里加 -psn_0_xxxxx；
// 从文件管理器打开时也可能带上文件路径。这些都不是本程序的参数，不能被
// flag 当作错误处理，否则双击图标只会闪一下什么都不发生。
func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("data", "", "账本数据目录，默认为系统用户数据目录下的 "+appName)
	showVersion := fs.Bool("version", false, "打印版本号后退出")
	capture := fs.String("capture", "", "开发用：界面加载后截图到该文件并退出")
	view := fs.String("view", "", "开发用：截图前切换到的页面（issues/entries/brokers/stats/ai/entry，或 entry/12 打开某条记录）")
	captureHeight := fs.Int("capture-height", 0, "开发用：截图时的窗口高度，0 表示默认")
	captureWidth := fs.Int("capture-width", 0, "开发用：截图时的窗口宽度，0 表示默认")
	apiAddr := fs.String("api-addr", httpapi.DefaultAddr, "本地接口监听地址，仅接受回环地址")
	noAPI := fs.Bool("no-api", false, "不启动本地接口")
	exportSkill := fs.String("export-skill", "", "把 agent skill 包导出到指定目录后退出，不打开窗口")

	// 解析失败时也保留已经解析出来的值：启动器多带的参数不应该让程序丢掉自己的参数。
	err := fs.Parse(filterArgs(args))
	return options{
		dataDir:       *dir,
		showVersion:   *showVersion,
		capturePath:   *capture,
		captureView:   *view,
		captureHeight: *captureHeight,
		captureWidth:  *captureWidth,
		apiAddr:       *apiAddr,
		noAPI:         *noAPI,
		exportSkill:   *exportSkill,
	}, err
}

// filterArgs 去掉不属于本程序的参数，也就是 -psn_ 开头的系统参数。
//
// 其余参数原样保留：flag 解析会在第一个非参数处停下，所以访达或文件管理器
// 附带的路径不会造成误判，而参数值里本身就可能是不带横线的路径。
func filterArgs(args []string) []string {
	kept := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-psn_") {
			continue
		}
		kept = append(kept, arg)
	}
	return kept
}

// startURL 返回窗口打开的地址。正常启动是首页，开发截图时带上标签页 hash。
func startURL(view string) string {
	switch view {
	case "issues", "entries", "brokers", "stats", "ai", "entry":
		return "/#" + view
	default:
		// entry/<id> 直接打开某条记录的抽屉，便于截图与排查单条数据；
		// entry/<id>/sales 再滚到卖出明细；issue/<申购代码> 打开新股详情；
		// stats/<年>-<月> 带着筛选条件进统计页。
		if strings.HasPrefix(view, "entry/") || strings.HasPrefix(view, "issue/") ||
			strings.HasPrefix(view, "stats/") {
			return "/#" + view
		}
		return "/"
	}
}

// startAPI 启动本地接口。
//
// 默认端口被占用时退到随机端口：端口冲突不该让 agent 完全接不上，实际地址会写
// 进发现文件，调用方总是以文件为准。
func startAPI(svc *api.Service, dataDir, addr string) *httpapi.Server {
	srv := httpapi.New(svc, dataDir, appTitle, displayVersion())
	// 常驻实例才写家目录下的固定指针，agent 靠它找到随机端口。
	srv.EnablePointer()
	if err := srv.Start(addr); err != nil {
		log.Printf("本地接口未能在 %s 上启动：%v", addr, err)
		if err := srv.Start("127.0.0.1:0"); err != nil {
			log.Printf("本地接口未能启动，agent 与脚本无法接入：%v", err)
			return nil
		}
	}
	return srv
}

// exportSkillOnly 只导出 skill 包，不打开窗口。
//
// 先起接口再导出：skill 里要写真实的接口地址，而地址由监听结果决定。
func exportSkillOnly(opts options) error {
	dir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		return fmt.Errorf("无法确定数据目录：%w", err)
	}
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		return fmt.Errorf("打开账本失败：%w", err)
	}
	defer st.Close()

	svc := api.New(st, source.NewManager(source.NewBSEPrimary(), source.NewBSEBackup()))
	srv := httpapi.New(svc, dir, appTitle, displayVersion())

	// 应用已经在跑时，直接用它写下的地址：既拿到真实的接口地址，也不用去抢
	// 端口，更不会覆盖正在使用的发现文件。
	if baseURL, ok := runningBaseURL(srv.DiscoveryPath()); ok {
		if err := srv.ExportSkillAt(opts.exportSkill, baseURL); err != nil {
			return err
		}
		fmt.Printf("已导出 skill 到 %s（接口 %s）\n", opts.exportSkill, baseURL)
		return nil
	}

	if err := srv.Start(opts.apiAddr); err != nil {
		if err := srv.Start("127.0.0.1:0"); err != nil {
			return fmt.Errorf("启动本地接口失败：%w", err)
		}
	}
	defer srv.Close()

	if err := srv.ExportSkill(opts.exportSkill); err != nil {
		return err
	}
	fmt.Printf("已导出 skill 到 %s\n", opts.exportSkill)
	return nil
}

// runningBaseURL 读取已有实例写下的接口地址。
//
// 只有发现文件存在且进程还在时才认：进程崩掉后留下的文件会让 agent 连到一个
// 没人监听的端口，那种地址写进 skill 里比空着更糟。
func runningBaseURL(discoveryPath string) (string, bool) {
	data, err := os.ReadFile(discoveryPath)
	if err != nil {
		return "", false
	}
	var found struct {
		BaseURL string `json:"baseUrl"`
		PID     int    `json:"pid"`
	}
	if err := json.Unmarshal(data, &found); err != nil || found.BaseURL == "" {
		return "", false
	}
	process, err := os.FindProcess(found.PID)
	if err != nil {
		return "", false
	}
	// 信号 0 只探测进程是否存在，不影响它。
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return "", false
	}
	return found.BaseURL, true
}

// captureAndQuit 等界面加载完成后把窗口内容存成图片再退出。
//
// 只用于开发和自动化检查：窗口在后台时也能自取截图，不依赖系统的屏幕
// 录制权限。
func captureAndQuit(win *mygo.Window, path string) {
	time.Sleep(6 * time.Second)
	defer mygo.App.Quit()
	png, err := win.CapturePage()
	if err != nil {
		log.Printf("截图失败：%v", err)
		return
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		log.Printf("写入截图失败：%v", err)
		return
	}
	log.Printf("已截图到 %s（%d 字节）", path, len(png))
}

// displayVersion 返回用于展示的版本号：构建时注入的优先，其次读应用配置。
func displayVersion() string {
	if version != "" {
		return version
	}
	if v := mygo.App.Version(); v != "" {
		return v
	}
	return "dev"
}

// logFile 是运行日志文件，进程退出时关闭；创建失败时为 nil。
var logFile *os.File

// setupLog 把日志写到数据目录下的 app.log，超过 1MB 时轮转一次。
//
// 双击启动的图形应用没有终端，标准错误无处可看，出问题时日志文件是唯一的
// 现场线索。
func setupLog(dir string) error {
	// 数据目录可能还不存在（第一次启动），日志文件要先于数据库创建。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "app.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	logFile = f
	log.SetOutput(f)
	log.Printf("启动 %s %s，数据目录 %s", appName, displayVersion(), dir)
	return nil
}

// closeLog 关闭日志文件，日志没建起来时什么也不做。
func closeLog() {
	if logFile != nil {
		_ = logFile.Close()
	}
}

// showFatal 在窗口可用后把启动错误告诉用户，而不是静默退出。
func showFatal(title string, cause error) {
	_, _ = mygo.Dialog.Message(mygo.MessageOptions{
		Type:    mygo.MessageError,
		Title:   title,
		Message: title,
		Detail:  cause.Error() + "\n\n账本数据保存在 " + defaultDataHint() + "，可整体复制备份后再重试。",
		Buttons: []string{"好"},
	})
	mygo.App.Quit()
}

// defaultDataHint 是默认数据目录的展示路径，用于提示文案。
func defaultDataHint() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return "系统用户数据目录下的 " + appName
	}
	return filepath.Join(base, appName)
}

// appMenu 构建菜单栏。
//
// macOS 的复制、粘贴、全选等快捷键靠菜单项转发给输入框，没有 Edit 菜单
// 时表单里的 Cmd+C / Cmd+V 会失灵，所以这里显式给出。
func appMenu() *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{Role: mygo.RoleAppMenu},
		{Role: mygo.RoleEditMenu},
		{Role: mygo.RoleWindowMenu},
	})
}

// resolveDataDir 返回账本数据目录。显式指定的目录优先；
// 否则用系统的用户数据目录，便于随系统备份。
func resolveDataDir(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// desktop 是界面需要的桌面能力：选择文件、读写备份。
//
// 与 api.Service 分开，是为了让账本逻辑不依赖窗口，便于测试与复用。
type desktop struct {
	service *api.Service
	api     *httpapi.Server
}

// SkillResult 是导出 skill 包的结果。
type SkillResult struct {
	Canceled bool   `json:"canceled"`
	Dir      string `json:"dir"`
}

// APIInfo 返回本地接口的接入信息，界面据此展示地址与令牌。
func (d *desktop) APIInfo(ctx context.Context) (httpapi.Info, error) {
	if d.api == nil {
		return httpapi.Info{}, fmt.Errorf("本地接口没有启动，重新打开应用即可恢复")
	}
	return d.api.Info(), nil
}

// RotateToken 换一个访问令牌，用于怀疑令牌被别处读走时。
func (d *desktop) RotateToken(ctx context.Context) (httpapi.Info, error) {
	if d.api == nil {
		return httpapi.Info{}, fmt.Errorf("本地接口没有启动，重新打开应用即可恢复")
	}
	if _, err := d.api.RotateToken(); err != nil {
		return httpapi.Info{}, fmt.Errorf("重置令牌失败：%w", err)
	}
	return d.api.Info(), nil
}

// ExportSkill 让用户选一个目录，把 agent skill 包写进去。
func (d *desktop) ExportSkill(ctx context.Context) (SkillResult, error) {
	if d.api == nil {
		return SkillResult{}, fmt.Errorf("本地接口没有启动，重新打开应用即可恢复")
	}
	defaultPath, err := os.UserHomeDir()
	if err != nil {
		defaultPath = d.api.DataPath()
	}
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:            mygo.CallerWindow(ctx),
		Title:             "选择 skill 导出目录",
		DefaultPath:       defaultPath,
		Message:           "会在这个目录下生成 SKILL.md、references/api.md 与 scripts/ipobook.sh，交给 agent 使用。",
		ButtonLabel:       "导出到此处",
		Directory:         true,
		CreateDirectories: true,
	})
	if err != nil {
		return SkillResult{}, fmt.Errorf("打开目录选择窗口失败：%w", err)
	}
	if len(paths) == 0 {
		return SkillResult{Canceled: true}, nil
	}
	if err := d.api.ExportSkill(paths[0]); err != nil {
		return SkillResult{}, err
	}
	return SkillResult{Dir: paths[0]}, nil
}

// BackupResult 是保存备份的结果。
type BackupResult struct {
	// Canceled 表示用户取消了选择。
	Canceled bool `json:"canceled"`
	// Path 是备份文件保存位置。
	Path string `json:"path"`
}

// RestoreResult 是恢复备份的结果。
type RestoreResult struct {
	Canceled bool `json:"canceled"`
	Imported int  `json:"imported"`
}

// SaveBackup 让用户选择位置，把账本导出成 JSON 文件。
func (d *desktop) SaveBackup(ctx context.Context) (BackupResult, error) {
	payload, err := d.service.ExportJSON(ctx)
	if err != nil {
		return BackupResult{}, err
	}
	defaultName := fmt.Sprintf("打新账本-%s.json", time.Now().Format(time.DateOnly))
	path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{
		Parent:      mygo.CallerWindow(ctx),
		Title:       "保存账本备份",
		DefaultPath: defaultName,
		Message:     "备份包含全部打新记录，可随时用「恢复」导入。",
		ButtonLabel: "保存",
		Filters:     []mygo.FileFilter{{Name: "备份文件", Extensions: []string{"json"}}},
	})
	if err != nil {
		return BackupResult{}, fmt.Errorf("打开保存窗口失败：%w", err)
	}
	if path == "" {
		return BackupResult{Canceled: true}, nil
	}
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		return BackupResult{}, fmt.Errorf("写入备份文件失败：%w", err)
	}
	return BackupResult{Path: path}, nil
}

// LoadBackup 让用户选择备份文件并导入，替换当前账本。
func (d *desktop) LoadBackup(ctx context.Context) (RestoreResult, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:  mygo.CallerWindow(ctx),
		Title:   "选择账本备份",
		Message: "恢复会用备份内容替换当前账本，请确认已保存现有数据。",
		Filters: []mygo.FileFilter{{Name: "备份文件", Extensions: []string{"json"}}},
	})
	if err != nil {
		return RestoreResult{}, fmt.Errorf("打开文件窗口失败：%w", err)
	}
	if len(paths) == 0 {
		return RestoreResult{Canceled: true}, nil
	}
	payload, err := os.ReadFile(paths[0])
	if err != nil {
		return RestoreResult{}, fmt.Errorf("读取备份文件失败：%w", err)
	}
	imported, err := d.service.ImportJSON(ctx, string(payload))
	if err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Imported: imported}, nil
}
