package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// PrimaryBaseURL 是北交所官网，作为主数据源。
	PrimaryBaseURL = "https://www.bse.cn"
	// BackupBaseURL 是北交所信息披露平台，作为备用数据源。
	BackupBaseURL = "https://www.bseinfo.net"

	// infoPath 是新股发行列表接口。
	infoPath = "/newShareController/infoResult.do"
	// refererPath 是接口对应的页面，接口要求带上它作为 Referer。
	refererPath = "/newshare/listofissues.html"

	// userAgent 与浏览器保持一致，避免被 CDN 当作脚本请求。
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"

	// defaultPages 是单次拉取的分页数。站点每页 20 条，6 页约覆盖最近一年。
	defaultPages = 6
	// maxPages 是分页上限，防止异常响应导致死循环。
	maxPages = 20
	// tokenTTL 是站点 cookie 的有效期。站点下发 max-age=300。
	tokenTTL = 4 * time.Minute
)

// cst 是北京时间时区。北交所的日期字段都是北京时间零点。
var cst = time.FixedZone("CST", 8*3600)

// BSEClient 读取北交所新股发行列表。
//
// 站点由 wswaf 保护，首次访问会返回一段设置 C3VK cookie 的脚本而不是正常
// 页面，带上该 cookie 重新请求即可放行，见 ensureToken。
type BSEClient struct {
	id       string
	label    string
	base     string
	maxPages int
	http     *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// NewBSEPrimary 创建主数据源（北交所官网）。
func NewBSEPrimary() *BSEClient {
	return newBSEClient("bse", "北交所官网", PrimaryBaseURL)
}

// NewBSEBackup 创建备用数据源（北交所信息披露平台）。
func NewBSEBackup() *BSEClient {
	return newBSEClient("bseinfo", "北交所信息披露平台", BackupBaseURL)
}

func newBSEClient(id, label, base string) *BSEClient {
	return &BSEClient{
		id:       id,
		label:    label,
		base:     strings.TrimRight(base, "/"),
		maxPages: defaultPages,
		http: &http.Client{
			Timeout: 30 * time.Second,
			// 站点的挑战是 307，自行处理重试，不交给客户端循环跟随。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ID 实现 Provider。
func (c *BSEClient) ID() string { return c.id }

// Label 实现 Provider。
func (c *BSEClient) Label() string { return c.label }

// Fetch 实现 Provider，拉取指定阶段的全部新股。
//
// 站点忽略 statetypes 参数、总是返回全量历史，所以按阶段过滤在本地完成：
// 尚未公布上市日、或上市日不早于今天的，视为仍在发行流程中。
func (c *BSEClient) Fetch(ctx context.Context, scope Scope) ([]Issue, error) {
	today := time.Now().In(cst).Format(time.DateOnly)
	issues := make([]Issue, 0, c.maxPages*20)
	seen := make(map[string]bool)
	for page := 0; page < c.maxPages; page++ {
		info, err := c.fetchPage(ctx, scope, page)
		if err != nil {
			return nil, err
		}
		for _, row := range info.Content {
			issue, ok := row.toIssue()
			if !ok || seen[issue.FxCode] {
				continue
			}
			seen[issue.FxCode] = true
			if !scope.match(issue, today) {
				continue
			}
			issues = append(issues, issue)
		}
		if len(info.Content) == 0 || info.LastPage || page+1 >= info.TotalPages {
			break
		}
	}
	return issues, nil
}

// match 报告该新股是否属于当前阶段。scope 为 ScopeAll 时全部保留。
func (s Scope) match(issue Issue, today string) bool {
	if s == ScopeAll {
		return true
	}
	// 上市日为空说明还没走完发行流程；上市日不早于今天说明仍在流程内。
	ongoing := issue.ListingDate == "" || issue.ListingDate >= today
	if s == ScopeIssuing {
		return ongoing
	}
	return !ongoing
}

// listInfo 是接口返回的分页信息。
type listInfo struct {
	Content    []issueRow `json:"content"`
	TotalPages int        `json:"totalPages"`
	LastPage   bool       `json:"lastPage"`
}

// fetchPage 拉取一页。遇到 cookie 挑战时刷新后重试一次。
func (c *BSEClient) fetchPage(ctx context.Context, scope Scope, page int) (listInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.ensureToken(ctx, attempt > 0)
		if err != nil {
			return listInfo{}, err
		}
		info, err := c.postPage(ctx, scope, page, token)
		if err == nil {
			return info, nil
		}
		if !errors.Is(err, errChallenge) {
			return listInfo{}, err
		}
		lastErr = err
	}
	return listInfo{}, fmt.Errorf("站点校验未通过：%w", lastErr)
}

// errChallenge 表示响应是站点的 cookie 挑战页。
var errChallenge = errors.New("站点返回 cookie 挑战")

// postPage 发送一次列表请求。
func (c *BSEClient) postPage(ctx context.Context, scope Scope, page int, token string) (listInfo, error) {
	form := url.Values{
		"statetypes":  {scope.statetype()},
		"page":        {strconv.Itoa(page)},
		"companyCode": {""},
		"isNewThree":  {"1"},
		"sortfield":   {"purchaseDate"},
		"sorttype":    {"desc"},
		"startTime":   {""},
		"endTime":     {""},
		"keyword":     {""},
	}
	endpoint := c.base + infoPath + "?callback=bseIpoBookCallback"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return listInfo{}, err
	}
	c.setHeaders(req, token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	body, err := c.do(req)
	if err != nil {
		return listInfo{}, err
	}
	if isChallenge(body) {
		return listInfo{}, errChallenge
	}
	var envelopes []struct {
		ListInfo listInfo `json:"listInfo"`
	}
	if err := decodeJSONP(body, &envelopes); err != nil {
		return listInfo{}, fmt.Errorf("解析新股列表失败：%w", err)
	}
	if len(envelopes) == 0 {
		return listInfo{}, nil
	}
	return envelopes[0].ListInfo, nil
}

// ensureToken 取得可用的站点 cookie。force 为真时忽略缓存重新获取。
func (c *BSEClient) ensureToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Since(c.tokenAt) < tokenTTL {
		return c.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/", nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(req, "")
	body, err := c.do(req)
	if err != nil {
		return "", err
	}
	token, ok := parseToken(body)
	if !ok {
		return "", errors.New("未能取得站点校验 cookie")
	}
	c.token = token
	c.tokenAt = time.Now()
	return token, nil
}

// do 执行请求并读取响应体。
func (c *BSEClient) do(req *http.Request) ([]byte, error) {
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 %s 失败：%w", c.label, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	// 307 也是站点校验的一种形态，交给上层刷新 cookie 后重试。
	if res.StatusCode == http.StatusTemporaryRedirect || res.StatusCode == http.StatusFound {
		return nil, errChallenge
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 HTTP %d", c.label, res.StatusCode)
	}
	return body, nil
}

// setHeaders 设置请求头，模拟浏览器的正常访问。
func (c *BSEClient) setHeaders(req *http.Request, token string) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", c.base+refererPath)
	if token != "" {
		req.Header.Set("Cookie", token)
	}
}

// isChallenge 判断响应是否为站点校验页。
func isChallenge(body []byte) bool {
	return bytes.Contains(body, []byte("document.cookie")) || bytes.Contains(body, []byte("_$"))
}

// parseToken 从校验页脚本里取出 "C3VK=xxxx" 形式的 cookie。
func parseToken(body []byte) (string, bool) {
	idx := bytes.Index(body, []byte("C3VK="))
	if idx < 0 {
		return "", false
	}
	rest := body[idx:]
	end := 0
	for end < len(rest) {
		ch := rest[end]
		if ch == ';' || ch == '"' || ch == '\'' || ch == ' ' || ch == '\\' {
			break
		}
		end++
	}
	if end <= len("C3VK=") {
		return "", false
	}
	return string(rest[:end]), true
}

// decodeJSONP 解析 JSON 或 JSONP 响应。
func decodeJSONP(body []byte, out any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return errors.New("响应为空")
	}
	if trimmed[0] == '[' || trimmed[0] == '{' {
		return json.Unmarshal(trimmed, out)
	}
	open := bytes.IndexByte(trimmed, '(')
	closing := bytes.LastIndexByte(trimmed, ')')
	if open < 1 || closing <= open {
		return errors.New("响应不是合法的 JSONP")
	}
	return json.Unmarshal(trimmed[open+1:closing], out)
}

// issueRow 是接口返回的一行原始数据。
//
// 价格、市盈率以 json.Number 接收，避免二进制浮点在解析阶段就引入误差；
// 日期既可能是 Java Date 对象，也可能是字符串，由 flexDate 统一处理。
type issueRow struct {
	FxCode          string      `json:"fxCode"`
	StockCode       string      `json:"stockCode"`
	StockName       string      `json:"stockName"`
	IssuePrice      json.Number `json:"issuePrice"`
	PeRatio         json.Number `json:"peRatio"`
	InitialIssueAmt json.Number `json:"initialIssueAmount"`
	PurchaseDate    flexDate    `json:"purchaseDate"`
	ResultDate      flexDate    `json:"issueResultDate"`
	ListingDate     flexDate    `json:"enterPremiumDate"`
}

// toIssue 把接口行转换为领域模型。缺少申购代码的行无法入账，直接丢弃。
func (r issueRow) toIssue() (Issue, bool) {
	fx := strings.TrimSpace(r.FxCode)
	if fx == "" {
		return Issue{}, false
	}
	return Issue{
		FxCode:       fx,
		StockCode:    strings.TrimSpace(r.StockCode),
		Name:         strings.TrimSpace(r.StockName),
		PriceFen:     parseFen(r.IssuePrice.String()),
		IssueShares:  parseWhole(r.InitialIssueAmt.String()),
		PEHundredths: parseFen(r.PeRatio.String()),
		PurchaseDate: r.PurchaseDate.day(),
		ResultDate:   r.ResultDate.day(),
		ListingDate:  r.ListingDate.day(),
	}, true
}

// parseFen 把 "4.48" 这类两位小数金额转换为以分为单位的整数，
// 遇到空值或非法值返回 0（表示未知，而不是零成本）。
func parseFen(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return 0
	}
	negative := strings.HasPrefix(raw, "-")
	raw = strings.TrimPrefix(raw, "-")
	intPart, fracPart, _ := strings.Cut(raw, ".")
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0
	}
	// 只保留两位小数，第三位四舍五入。
	frac := (fracPart + "000")[:3]
	cents, err := strconv.ParseInt(frac[:2], 10, 64)
	if err != nil {
		return 0
	}
	if frac[2] >= '5' {
		cents++
	}
	total := whole*100 + cents
	if negative {
		total = -total
	}
	return total
}

// parseWhole 解析整数型数量字段，非法值返回 0。
func parseWhole(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return 0
	}
	if intPart, _, ok := strings.Cut(raw, "."); ok {
		raw = intPart
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// flexDate 兼容 Java Date 对象与日期字符串两种日期形态。
type flexDate struct {
	value string
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *flexDate) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		f.value = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		f.value = normalizeDay(s)
		return nil
	}
	var obj struct {
		Time int64 `json:"time"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return err
	}
	if obj.Time <= 0 {
		f.value = ""
		return nil
	}
	f.value = time.UnixMilli(obj.Time).In(cst).Format(time.DateOnly)
	return nil
}

// day 返回 YYYY-MM-DD 形式的日期，未知时为空。
func (f flexDate) day() string { return f.value }

// normalizeDay 把常见的日期字符串统一成 YYYY-MM-DD。
func normalizeDay(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.DateOnly, "2006/01/02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.DateOnly)
		}
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
