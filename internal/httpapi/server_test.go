package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wowh/ipo-book/internal/api"
	"github.com/wowh/ipo-book/internal/source"
	"github.com/wowh/ipo-book/internal/store"
)

// fakeProvider 提供固定的新股列表，避免测试依赖网络。
type fakeProvider struct{}

func (fakeProvider) Label() string { return "测试源" }
func (fakeProvider) ID() string    { return "fake" }

func (fakeProvider) Fetch(ctx context.Context, scope source.Scope) ([]source.Issue, error) {
	return []source.Issue{
		{FxCode: "920306", StockCode: "874158", Name: "天元重工", PriceFen: 448, PurchaseDate: "2026-10-20"},
		{FxCode: "920285", StockCode: "874235", Name: "远大健科", PriceFen: 2030, PurchaseDate: "2026-10-14", ListingDate: "2020-10-23"},
	}, nil
}

// newTestServer 起一个真实监听的接口服务，连同临时账本一起清理。
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("打开账本：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := api.New(st, source.NewManager(fakeProvider{}))
	srv := New(svc, dir, "打新账本", "test")
	if err := srv.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("启动接口：%v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// call 发一次请求，返回状态码与解析后的 JSON。
func call(t *testing.T, srv *Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.BaseURL()+path, reader)
	if err != nil {
		t.Fatalf("构造请求：%v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s：%v", method, path, err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读取响应：%v", err)
	}
	decoded := map[string]any{}
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("%s %s 响应不是 JSON：%s", method, path, payload)
		}
	}
	return res.StatusCode, decoded
}

func TestAuthAndGuards(t *testing.T) {
	srv := newTestServer(t)

	if code, _ := call(t, srv, http.MethodGet, "/api/health", "", ""); code != http.StatusUnauthorized {
		t.Errorf("无令牌访问状态码 = %d，want 401", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/api/health", "wrong-token", ""); code != http.StatusUnauthorized {
		t.Errorf("错误令牌状态码 = %d，want 401", code)
	}
	code, body := call(t, srv, http.MethodGet, "/api/health", srv.Token(), "")
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("带令牌访问 = %d %v", code, body)
	}

	// 浏览器跨站请求一律拒绝，即使令牌正确。
	req, _ := http.NewRequest(http.MethodGet, srv.BaseURL()+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	req.Header.Set("Origin", "https://example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("带 Origin 的请求：%v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("带 Origin 状态码 = %d，want 403", res.StatusCode)
	}

	// Host 不是本机时也拒绝（防 DNS rebinding）。
	req, _ = http.NewRequest(http.MethodGet, srv.BaseURL()+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Token())
	req.Host = "evil.example.com"
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("改写 Host 的请求：%v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("非本机 Host 状态码 = %d，want 403", res.StatusCode)
	}
}

func TestDiscoveryFile(t *testing.T) {
	srv := newTestServer(t)
	payload, err := os.ReadFile(srv.DiscoveryPath())
	if err != nil {
		t.Fatalf("读取发现文件：%v", err)
	}
	var found discovery
	if err := json.Unmarshal(payload, &found); err != nil {
		t.Fatalf("发现文件不是 JSON：%v", err)
	}
	if found.BaseURL != srv.BaseURL() || found.Token != srv.Token() {
		t.Fatalf("发现文件内容 = %+v", found)
	}
	if info, err := os.Stat(srv.DiscoveryPath()); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("发现文件权限 = %v，want 0600", info.Mode().Perm())
	}

	srv.Close()
	if _, err := os.Stat(srv.DiscoveryPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("退出后应删除发现文件，err = %v", err)
	}
}

func TestEntryLifecycle(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()

	code, broker := call(t, srv, http.MethodPost, "/api/brokers", token,
		`{"name":"华泰证券","account":"主账户","commissionWan":"2.5","minCommission":"5","annualRatePct":"3.85"}`)
	if code != http.StatusCreated {
		t.Fatalf("新增账户 = %d %v", code, broker)
	}
	brokerID := int64(broker["id"].(float64))
	// 万分之 2.5 = 250 ppm，3.85% = 38500 ppm，5 元 = 500 分。
	if broker["commissionPpm"].(float64) != 250 || broker["annualRatePpm"].(float64) != 38500 ||
		broker["minCommissionFen"].(float64) != 500 {
		t.Fatalf("费率换算 = %v", broker)
	}

	body := `{"brokerId":` + formatInt(brokerID) + `,"fxCode":"920306","name":"天元重工",
		"issuePrice":"4.48","purchaseDate":"2026-10-20","applyShares":1000}`
	code, entry := call(t, srv, http.MethodPost, "/api/entries", token, body)
	if code != http.StatusCreated {
		t.Fatalf("新增记录 = %d %v", code, entry)
	}
	id := int64(entry["id"].(float64))
	if entry["issuePriceFen"].(float64) != 448 {
		t.Errorf("发行价换算 = %v，want 448 分", entry["issuePriceFen"])
	}
	if entry["status"] != "已申购" {
		t.Errorf("状态 = %v，want 已申购", entry["status"])
	}

	// 只改中签数量，其他字段保持原样。
	code, patched := call(t, srv, http.MethodPatch, "/api/entries/"+formatInt(id), token, `{"allottedShares":500}`)
	if code != http.StatusOK {
		t.Fatalf("局部更新 = %d %v", code, patched)
	}
	if patched["allottedShares"].(float64) != 500 || patched["applyShares"].(float64) != 1000 {
		t.Fatalf("局部更新结果 = %v", patched)
	}
	if patched["issuePriceFen"].(float64) != 448 {
		t.Errorf("未提交的字段被改动了：%v", patched["issuePriceFen"])
	}
	if patched["status"] != "已中签" {
		t.Errorf("状态 = %v，want 已中签", patched["status"])
	}

	// 卖出后净收益要扣掉估算的卖出费用与资金成本。
	code, sold := call(t, srv, http.MethodPatch, "/api/entries/"+formatInt(id), token,
		`{"sellPrice":"6.20","soldShares":500,"sellDate":"2026-10-30"}`)
	if code != http.StatusOK {
		t.Fatalf("卖出更新 = %d %v", code, sold)
	}
	if sold["status"] != "已卖出" {
		t.Errorf("状态 = %v，want 已卖出", sold["status"])
	}
	// 收入 500×6.20=310000 分，成本 500×4.48=224000，佣金按最低 500 分，
	// 资金成本 448000×38500×2÷(1e6×365)=95 分 → 310000-224000-595=85405 分。
	if sold["realizedFen"].(float64) != 85405 {
		t.Errorf("净收益 = %v，want 85405", sold["realizedFen"])
	}
	// 老的单笔写法内部折算成一条明细，读出来是同一套结构。
	if sold["saleCount"].(float64) != 1 || len(sold["sales"].([]any)) != 1 {
		t.Fatalf("单笔写法应折成一条明细：%v", sold["sales"])
	}
	if sale := sold["sales"].([]any)[0].(map[string]any); sale["date"] != "2026-10-30" ||
		sale["shares"].(float64) != 500 || sale["priceFen"].(float64) != 620 {
		t.Errorf("折算出的明细 = %v", sale)
	}

	code, list := call(t, srv, http.MethodGet, "/api/entries?fxCode=920306", token, "")
	if code != http.StatusOK || list["count"].(float64) != 1 {
		t.Fatalf("按代码过滤 = %d %v", code, list)
	}

	code, summary := call(t, srv, http.MethodGet, "/api/summary", token, "")
	if code != http.StatusOK {
		t.Fatalf("汇总 = %d", code)
	}
	if summary["realizedFen"].(float64) != 85405 || summary["appliedIssues"].(float64) != 1 {
		t.Fatalf("汇总内容 = %v", summary)
	}

	code, _ = call(t, srv, http.MethodDelete, "/api/entries/"+formatInt(id), token, "")
	if code != http.StatusOK {
		t.Fatalf("删除记录 = %d", code)
	}
	code, _ = call(t, srv, http.MethodDelete, "/api/entries/"+formatInt(id), token, "")
	if code != http.StatusNotFound {
		t.Errorf("删除不存在的记录 = %d，want 404", code)
	}
}

// TestEntrySalesEndpoints 覆盖卖出明细：数组写入、单笔追加、修改与删除。
func TestEntrySalesEndpoints(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()

	code, broker := call(t, srv, http.MethodPost, "/api/brokers", token,
		`{"name":"华泰证券","commissionWan":"2.5","minCommission":"5"}`)
	if code != http.StatusCreated {
		t.Fatalf("新增账户 = %d %v", code, broker)
	}
	brokerID := int64(broker["id"].(float64))

	// 两笔卖出：一笔给成交价，一笔给成交金额与费用。
	body := `{"brokerId":` + formatInt(brokerID) + `,"fxCode":"920306","name":"天元重工",
		"issuePrice":"4.48","allottedShares":1500,
		"sales":[{"date":"2026-10-28","shares":400,"price":"6.20"},
		         {"date":"2026-10-30","shares":600,"amount":"3720.00","fee":"2.50"}]}`
	code, entry := call(t, srv, http.MethodPost, "/api/entries", token, body)
	if code != http.StatusCreated {
		t.Fatalf("新增记录 = %d %v", code, entry)
	}
	id := int64(entry["id"].(float64))
	if entry["saleCount"].(float64) != 2 {
		t.Fatalf("卖出笔数 = %v", entry["saleCount"])
	}
	// 400×6.20 = 2480.00，加 3720.00 共 6200.00 元。
	if entry["sellAmountFen"].(float64) != 620000 {
		t.Errorf("成交金额 = %v，want 620000", entry["sellAmountFen"])
	}
	if entry["soldShares"].(float64) != 1000 || entry["status"] != "部分卖出" {
		t.Errorf("股数/状态 = %v/%v", entry["soldShares"], entry["status"])
	}
	// 费用：第一笔按最低 5 元，第二笔用了填的 2.50 元。
	if entry["sellFeeFen"].(float64) != 750 {
		t.Errorf("卖出费用 = %v，want 750", entry["sellFeeFen"])
	}
	sales := entry["sales"].([]any)
	if first := sales[0].(map[string]any); first["shares"].(float64) != 400 ||
		first["priceFen"].(float64) != 620 || first["amountFen"].(float64) != 248000 {
		t.Errorf("第一笔 = %v", first)
	}

	// 追加一笔，只给成交价，费用留空由佣金率算。
	path := "/api/entries/" + formatInt(id) + "/sales"
	code, appended := call(t, srv, http.MethodPost, path, token,
		`{"date":"2026-10-31","shares":500,"price":"6.00"}`)
	if code != http.StatusCreated {
		t.Fatalf("追加卖出 = %d %v", code, appended)
	}
	if appended["saleCount"].(float64) != 3 || appended["soldShares"].(float64) != 1500 {
		t.Fatalf("追加后 = %v 笔 %v 股", appended["saleCount"], appended["soldShares"])
	}
	if appended["status"] != "已卖出" {
		t.Errorf("状态 = %v，want 已卖出", appended["status"])
	}
	list := appended["sales"].([]any)
	lastID := int64(list[len(list)-1].(map[string]any)["id"].(float64))
	if lastID == 0 {
		t.Fatal("追加的明细没有 ID，无法再定位")
	}

	// 改这一笔的成交价。
	code, patched := call(t, srv, http.MethodPatch,
		path+"/"+formatInt(lastID), token, `{"date":"2026-10-31","shares":500,"price":"6.10"}`)
	if code != http.StatusOK {
		t.Fatalf("修改卖出 = %d %v", code, patched)
	}
	// 920000 里把 300000 换成 305000。
	if patched["sellAmountFen"].(float64) != 925000 {
		t.Errorf("修改后成交金额 = %v，want 925000", patched["sellAmountFen"])
	}

	// 删掉第一笔：剩两笔，1100 股。
	code, removed := call(t, srv, http.MethodDelete,
		path+"/"+formatInt(int64(sales[0].(map[string]any)["id"].(float64))), token, "")
	if code != http.StatusOK {
		t.Fatalf("删除卖出 = %d %v", code, removed)
	}
	if removed["saleCount"].(float64) != 2 || removed["soldShares"].(float64) != 1100 {
		t.Fatalf("删除后 = %v 笔 %v 股", removed["saleCount"], removed["soldShares"])
	}
	// 372000 + 305000
	if removed["sellAmountFen"].(float64) != 677000 {
		t.Errorf("删除后成交金额 = %v，want 677000", removed["sellAmountFen"])
	}

	// 删一个不存在的明细。
	code, body404 := call(t, srv, http.MethodDelete, path+"/999999", token, "")
	if code != http.StatusNotFound {
		t.Errorf("删除不存在的明细 = %d %v，want 404", code, body404)
	}

	// 一笔卖出既没价格也没金额 → 400。
	code, bad := call(t, srv, http.MethodPost, path, token, `{"date":"2026-11-01","shares":100}`)
	if code != http.StatusBadRequest || !strings.Contains(bad["error"].(string), "成交价") {
		t.Errorf("缺少价格 = %d %v", code, bad)
	}

	// sales 与老的单笔字段同时给 → 400，不去猜调用方本意。
	code, mixed := call(t, srv, http.MethodPatch, "/api/entries/"+formatInt(id), token,
		`{"sales":[{"shares":100,"price":"6.00"}],"soldShares":100}`)
	if code != http.StatusBadRequest {
		t.Errorf("两种写法混用 = %d %v，want 400", code, mixed)
	}

	// 明细合计超过中签数量 → 400。
	code, over := call(t, srv, http.MethodPost, "/api/entries", token,
		`{"brokerId":`+formatInt(brokerID)+`,"fxCode":"920285","issuePrice":"20.30","allottedShares":100,
		"sales":[{"shares":100,"price":"21.00"},{"shares":100,"price":"21.00"}]}`)
	if code != http.StatusBadRequest {
		t.Errorf("明细合计超过中签 = %d %v，want 400", code, over)
	}
}

func TestEntryValidation(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()
	code, broker := call(t, srv, http.MethodPost, "/api/brokers", token, `{"name":"中信证券"}`)
	if code != http.StatusCreated {
		t.Fatalf("新增账户 = %d %v", code, broker)
	}
	brokerID := int64(broker["id"].(float64))
	base := `"brokerId":` + formatInt(brokerID) + `,"fxCode":"920306","issuePrice":"4.48"`

	code, body := call(t, srv, http.MethodPost, "/api/entries", token, `{`+base+`,"issuePriceYuan":"4.48"}`)
	if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "未知字段") {
		t.Errorf("未知字段 = %d %v", code, body)
	}
	// issuePriceYuan 这种写法不对，错误里要给出可用字段，便于 agent 自我修正。
	if msg, _ := body["error"].(string); !strings.Contains(msg, "issuePrice") {
		t.Errorf("错误提示缺少可用字段：%s", msg)
	}

	code, body = call(t, srv, http.MethodPost, "/api/entries", token, `{`+base+`,"status":"已卖出"}`)
	if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "只读") {
		t.Errorf("写入派生字段 = %d %v", code, body)
	}

	code, body = call(t, srv, http.MethodPost, "/api/entries", token, `{`+base+`,"account":"华泰"}`)
	if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "brokerId") {
		t.Errorf("提交 account = %d %v", code, body)
	}

	// 同一账户同一只新股只能一条。
	if code, _ := call(t, srv, http.MethodPost, "/api/entries", token, `{`+base+`}`); code != http.StatusCreated {
		t.Fatalf("首次录入 = %d", code)
	}
	code, body = call(t, srv, http.MethodPost, "/api/entries", token, `{`+base+`}`)
	if code != http.StatusConflict {
		t.Errorf("重复录入 = %d %v，want 409", code, body)
	}

	// 卖出数量超过中签数量（老的单笔写法也走同一套校验）。
	code, _ = call(t, srv, http.MethodPost, "/api/entries", token,
		`{"brokerId":`+formatInt(brokerID)+`,"fxCode":"920285","issuePrice":"20.30","allottedShares":100,"soldShares":200,"sellPrice":"21.00"}`)
	if code != http.StatusBadRequest {
		t.Errorf("卖出超过中签 = %d，want 400", code)
	}
}

func TestBrokerRoutes(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()
	code, first := call(t, srv, http.MethodPost, "/api/brokers", token, `{"name":"华泰证券","account":"主账户"}`)
	if code != http.StatusCreated {
		t.Fatalf("新增账户 = %d %v", code, first)
	}
	id := int64(first["id"].(float64))

	// 只改年化利率。
	code, patched := call(t, srv, http.MethodPatch, "/api/brokers/"+formatInt(id), token, `{"annualRatePct":"4.2"}`)
	if code != http.StatusOK {
		t.Fatalf("修改账户 = %d %v", code, patched)
	}
	if patched["annualRatePpm"].(float64) != 42000 || patched["name"] != "华泰证券" {
		t.Fatalf("修改结果 = %v", patched)
	}

	// 重名账户。
	code, _ = call(t, srv, http.MethodPost, "/api/brokers", token, `{"name":"华泰证券","account":"主账户"}`)
	if code != http.StatusConflict {
		t.Errorf("重名账户 = %d，want 409", code)
	}
	// 缺券商名称。
	code, _ = call(t, srv, http.MethodPost, "/api/brokers", token, `{"account":"无名"}`)
	if code != http.StatusBadRequest && code != http.StatusConflict {
		t.Errorf("缺名称 = %d，want 400/409", code)
	}

	if code, _ := call(t, srv, http.MethodDelete, "/api/brokers/"+formatInt(id), token, ""); code != http.StatusOK {
		t.Errorf("删除账户 = %d", code)
	}
	if code, _ := call(t, srv, http.MethodPatch, "/api/brokers/"+formatInt(id), token, `{"note":"x"}`); code != http.StatusNotFound {
		t.Errorf("修改已删除账户 = %d，want 404", code)
	}
}

func TestIssueFilters(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()
	if code, body := call(t, srv, http.MethodPost, "/api/issues/refresh", token, `{"scope":"all"}`); code != http.StatusOK {
		t.Fatalf("同步新股 = %d %v", code, body)
	}

	// 一只还在发行，一只已上市。
	code, body := call(t, srv, http.MethodGet, "/api/issues?scope=issuing", token, "")
	if code != http.StatusOK || body["count"].(float64) != 1 {
		t.Fatalf("issuing = %d %v", code, body)
	}
	if code, body = call(t, srv, http.MethodGet, "/api/issues?scope=done", token, ""); code != http.StatusOK ||
		body["count"].(float64) != 1 {
		t.Fatalf("done = %d %v", code, body)
	}
	if code, body = call(t, srv, http.MethodGet, "/api/issues?q=天元", token, ""); code != http.StatusOK ||
		body["count"].(float64) != 1 {
		t.Fatalf("按名称搜索 = %d %v", code, body)
	}
	if code, _ = call(t, srv, http.MethodGet, "/api/issues?scope=nope", token, ""); code != http.StatusBadRequest {
		t.Errorf("非法 scope = %d，want 400", code)
	}
}

func TestBackupRoutes(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()
	call(t, srv, http.MethodPost, "/api/brokers", token, `{"name":"华泰证券","account":"主账户"}`)

	// 导出的是纯 JSON 对象，不是字符串包一层。
	req, _ := http.NewRequest(http.MethodGet, srv.BaseURL()+"/api/backup", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("导出备份：%v", err)
	}
	payload, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("导出备份 = %d", res.StatusCode)
	}
	var backup map[string]any
	if err := json.Unmarshal(payload, &backup); err != nil {
		t.Fatalf("备份不是 JSON：%v", err)
	}
	if _, ok := backup["brokers"]; !ok {
		t.Fatalf("备份缺少券商账户：%v", backup)
	}

	// 恢复必须显式确认，避免误调用把账本冲掉。
	if code, _ := call(t, srv, http.MethodPost, "/api/backup/restore", token, string(payload)); code != http.StatusBadRequest {
		t.Errorf("未确认的恢复 = %d，want 400", code)
	}
	code, body := call(t, srv, http.MethodPost, "/api/backup/restore?confirm=replace", token, string(payload))
	if code != http.StatusOK {
		t.Fatalf("恢复备份 = %d %v", code, body)
	}
}

func TestSkillExport(t *testing.T) {
	srv := newTestServer(t)
	dir := filepath.Join(t.TempDir(), "ipo-book")
	if err := srv.ExportSkill(dir); err != nil {
		t.Fatalf("导出 skill：%v", err)
	}

	for _, name := range []string{"SKILL.md", "references/api.md", "scripts/ipobook.sh"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("缺少 %s：%v", name, err)
		}
	}
	doc, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	if !strings.HasPrefix(text, "---\nname: ipo-book\n") {
		t.Errorf("SKILL.md 缺少 frontmatter：%.60s", text)
	}
	if strings.Contains(text, "{{") {
		t.Error("SKILL.md 里还有未替换的占位符")
	}
	if !strings.Contains(text, srv.BaseURL()) {
		t.Errorf("SKILL.md 没写接口地址 %s", srv.BaseURL())
	}
	if !strings.Contains(text, srv.DiscoveryPath()) {
		t.Error("SKILL.md 没写发现文件路径")
	}

	script := filepath.Join(dir, "scripts", "ipobook.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("脚本没有执行权限：%v", info.Mode().Perm())
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), srv.DiscoveryPath()) {
		t.Error("脚本没写发现文件路径")
	}

	// 通过接口导出到默认位置也应该工作。
	code, result := call(t, srv, http.MethodPost, "/api/skill/export", srv.Token(), `{}`)
	if code != http.StatusOK {
		t.Fatalf("接口导出 = %d %v", code, result)
	}
	if _, err := os.Stat(filepath.Join(srv.SkillDir(), "SKILL.md")); err != nil {
		t.Fatalf("默认目录没有 SKILL.md：%v", err)
	}
	if code, body := call(t, srv, http.MethodGet, "/api/skill", srv.Token(), ""); code != http.StatusOK ||
		body["exported"] != true {
		t.Fatalf("skill 信息 = %d %v", code, body)
	}
}

func TestRotateToken(t *testing.T) {
	srv := newTestServer(t)
	old := srv.Token()
	fresh, err := srv.RotateToken()
	if err != nil {
		t.Fatalf("重置令牌：%v", err)
	}
	if fresh == old || len(fresh) < 32 {
		t.Fatalf("新令牌 = %q", fresh)
	}
	if code, _ := call(t, srv, http.MethodGet, "/api/health", old, ""); code != http.StatusUnauthorized {
		t.Errorf("旧令牌仍可用：%d", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/api/health", fresh, ""); code != http.StatusOK {
		t.Errorf("新令牌不可用：%d", code)
	}
	// 发现文件要同步成新令牌，否则脚本拿到的还是旧的。
	payload, err := os.ReadFile(srv.DiscoveryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), fresh) {
		t.Error("发现文件没有更新令牌")
	}
}

func TestUnknownPath(t *testing.T) {
	srv := newTestServer(t)
	code, body := call(t, srv, http.MethodGet, "/api/nope", srv.Token(), "")
	if code != http.StatusNotFound {
		t.Fatalf("未知路径 = %d", code)
	}
	if !strings.Contains(body["error"].(string), "GET /api") {
		t.Errorf("未知路径提示 = %v", body["error"])
	}
}

func TestLegacyLedgerImportRouteRemoved(t *testing.T) {
	srv := newTestServer(t)
	code, _ := call(t, srv, http.MethodPost, "/api/import/ipo-ledger", srv.Token(), `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("旧账本导入路由仍可用：HTTP %d", code)
	}
}

// formatInt 把 int64 转成十进制文本，测试里拼 JSON 用。
func formatInt(v int64) string { return strconv.FormatInt(v, 10) }

// TestSkillScriptBraces 检查脚本里紧挨着中文的变量都写了花括号。
//
// bash 遇到 `$candidate（` 会把全角括号当成变量名的一部分，报「unbound
// variable」；这类错误只在中文提示里出现，跑一遍导出的脚本才看得到，所以
// 在模板层面先卡住。
func TestSkillScriptBraces(t *testing.T) {
	body, err := templates.ReadFile("templates/scripts/ipobook.sh")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*[^\x00-\x7f]`)
	for _, match := range re.FindAllString(string(body), -1) {
		t.Errorf("变量紧接非 ASCII 字符，需要写成 ${...}：%q", match)
	}
}

// TestStatsEndpoint 检查 /api/stats 的筛选与按年、按月拆分。
func TestStatsEndpoint(t *testing.T) {
	srv := newTestServer(t)
	token := srv.Token()

	_, broker := call(t, srv, http.MethodPost, "/api/brokers", token,
		`{"name":"华泰证券","account":"主账户","commissionWan":"2.5","minCommission":"5","annualRatePct":"3.85"}`)
	brokerID := formatInt(int64(broker["id"].(float64)))

	mk := func(fxCode, purchaseDate string, sales string) {
		t.Helper()
		body := `{"brokerId":` + brokerID + `,"fxCode":"` + fxCode + `","name":"测试新股",
			"issuePrice":"10","purchaseDate":"` + purchaseDate + `","applyShares":1000,
			"allottedShares":500,"sales":` + sales + `}`
		if code, payload := call(t, srv, http.MethodPost, "/api/entries", token, body); code != http.StatusCreated {
			t.Fatalf("新增记录 %s = %d %v", fxCode, code, payload)
		}
	}
	mk("920001", "2026-10-20", `[{"date":"2026-10-28","shares":500,"price":"12.00","fee":"5.00"}]`)
	mk("920002", "2025-12-15", `[]`)

	code, stats := call(t, srv, http.MethodGet, "/api/stats", token, "")
	if code != http.StatusOK {
		t.Fatalf("全部统计 = %d %v", code, stats)
	}
	summary := stats["summary"].(map[string]any)
	if summary["entryCount"].(float64) != 2 {
		t.Errorf("entryCount = %v, want 2", summary["entryCount"])
	}
	years := stats["years"].([]any)
	if len(years) != 2 || years[0].(float64) != 2026 || years[1].(float64) != 2025 {
		t.Errorf("years = %v, want [2026 2025]", years)
	}
	byYear := stats["byYear"].([]any)
	if len(byYear) != 2 {
		t.Fatalf("byYear 组数 = %d, want 2", len(byYear))
	}
	first := byYear[0].(map[string]any)
	if first["key"] != "2026" || first["saleCount"].(float64) != 1 {
		t.Errorf("2026 年分组 = %v", first)
	}
	byMonth := stats["byMonth"].([]any)
	if len(byMonth) != 2 || byMonth[0].(map[string]any)["key"] != "2026-10" {
		t.Errorf("byMonth = %v, want 2026-10 在前", byMonth)
	}
	// details 是逐条明细：一行一条记录，点某个月看细节就靠它。
	details := stats["details"].([]any)
	if len(details) != 2 {
		t.Fatalf("details 条数 = %d, want 2", len(details))
	}
	if got := details[0].(map[string]any)["purchaseDate"]; got != "2026-10-20" {
		t.Errorf("details[0].purchaseDate = %v, want 2026-10-20（申购日从新到旧）", got)
	}

	// 按年份筛选：只剩一条，且没有卖出金额。
	if code, stats := call(t, srv, http.MethodGet, "/api/stats?year=2025", token, ""); code != http.StatusOK {
		t.Fatalf("按年统计 = %d %v", code, stats)
	} else if got := stats["summary"].(map[string]any)["entryCount"].(float64); got != 1 {
		t.Errorf("2025 年 entryCount = %v, want 1", got)
	} else if bm := stats["byMonth"].([]any); len(bm) != 2 {
		// 月度列表是翻账本的入口，不吃年份筛选：筛了 2025 年，2026-10 仍要在列表里。
		t.Errorf("byMonth 组数 = %d, want 2（不受年份筛选影响）", len(bm))
	}
	// 按月份筛选。
	if code, stats := call(t, srv, http.MethodGet, "/api/stats?year=2026&month=10", token, ""); code != http.StatusOK {
		t.Fatalf("按月统计 = %d %v", code, stats)
	} else if got := stats["summary"].(map[string]any)["entryCount"].(float64); got != 1 {
		t.Errorf("2026-10 entryCount = %v, want 1", got)
	} else if bm := stats["byMonth"].([]any); len(bm) != 2 {
		t.Errorf("选中某个月后 byMonth 组数 = %d, want 2", len(bm))
	}
	// 参数不合法时给出 400，而不是静默当成不筛选。
	if code, _ := call(t, srv, http.MethodGet, "/api/stats?year=abc", token, ""); code != http.StatusBadRequest {
		t.Errorf("year=abc 状态码 = %d, want 400", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/api/stats?month=13", token, ""); code != http.StatusBadRequest {
		t.Errorf("month=13 状态码 = %d, want 400", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/api/stats", "", ""); code != http.StatusUnauthorized {
		t.Errorf("无令牌状态码 = %d, want 401", code)
	}
}
