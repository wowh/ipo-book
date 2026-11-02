// Package api 是前端与本地账本之间的接口层。
//
// 每个方法都对应一次前端调用，返回结构直接给界面使用：金额一律是整数分，
// 由前端格式化，避免把浮点数带进展示环节。错误信息面向用户，说明「发生了什么、
// 可以怎么做」，不暴露底层调用细节。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wowh/ipo-book/internal/ledger"
	"github.com/wowh/ipo-book/internal/source"
	"github.com/wowh/ipo-book/internal/store"
)

// Service 是绑定到前端的服务。
type Service struct {
	store   *store.Store
	sources *source.Manager

	mu       sync.Mutex
	lastMeta Meta
	// rev 在数据被改动时自增。桌面界面靠它发现「别的入口改过账本」，例如
	// agent 通过本地 HTTP 接口写入后，窗口需要重新读取。
	rev int64
}

// New 创建服务。
func New(st *store.Store, sources *source.Manager) *Service {
	return &Service{store: st, sources: sources}
}

// Meta 描述数据状态，用于界面顶部的来源与时间提示。
type Meta struct {
	// IssuesUpdatedAt 是最近一次同步新股列表的时间（RFC3339）。
	IssuesUpdatedAt string `json:"issuesUpdatedAt"`
	// SourceLabel 是本次数据的来源名称。
	SourceLabel string `json:"sourceLabel"`
	// Fallback 表示本次数据来自备用源。
	Fallback bool `json:"fallback"`
	// Warning 是降级提示，正常情况下为空。
	Warning string `json:"warning"`
	// DataPath 是账本数据文件位置，便于用户自行备份。
	DataPath string `json:"dataPath"`
}

// Snapshot 是界面渲染所需的完整数据。
type Snapshot struct {
	Issues  []source.Issue  `json:"issues"`
	Brokers []ledger.Broker `json:"brokers"`
	Entries []ledger.View   `json:"entries"`
	Summary ledger.Summary  `json:"summary"`
	Meta    Meta            `json:"meta"`
}

// RefreshResult 是一次新股同步的结果。
type RefreshResult struct {
	// Count 是本次写入缓存的新股数量。
	Count int `json:"count"`
	// Added 是列表中原本没有、本次新增的数量。
	Added   int            `json:"added"`
	Meta    Meta           `json:"meta"`
	Summary ledger.Summary `json:"summary"`
}

// Snapshot 读取当前账本与新股缓存，不访问网络。
func (s *Service) Snapshot(ctx context.Context) (Snapshot, error) {
	issues, err := s.store.ListIssues(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("读取新股列表失败：%w", err)
	}
	brokers, err := s.Brokers(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	views, err := s.Entries(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	raw := make([]ledger.Entry, 0, len(views))
	for _, v := range views {
		raw = append(raw, v.Entry)
	}

	s.mu.Lock()
	meta := s.lastMeta
	s.mu.Unlock()
	if meta.IssuesUpdatedAt == "" {
		meta.IssuesUpdatedAt = s.store.IssuesUpdatedAt(ctx)
	}
	meta.DataPath = s.storePath()

	return Snapshot{
		Issues:  issues,
		Brokers: brokers,
		Entries: views,
		Summary: ledger.Summarize(raw),
		Meta:    meta,
	}, nil
}

// Stats 按条件汇总收益，供统计页筛选与 agent 查询。
//
// 筛选一律看「申购日」：一只新股从申购到卖出可能跨月，整批算在申购当月，
// 这样各月、各年的数字相加正好等于总计，不会因为跨月卖出被算两次。
func (s *Service) Stats(ctx context.Context, filter ledger.StatsFilter) (ledger.Stats, error) {
	if err := filter.Valid(); err != nil {
		return ledger.Stats{}, err
	}
	entries, err := s.store.ListEntries(ctx)
	if err != nil {
		return ledger.Stats{}, fmt.Errorf("读取账本失败：%w", err)
	}
	return ledger.BuildStats(entries, filter), nil
}

// Brokers 返回全部券商账户。
func (s *Service) Brokers(ctx context.Context) ([]ledger.Broker, error) {
	brokers, err := s.store.ListBrokers(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取券商账户失败：%w", err)
	}
	return brokers, nil
}

// Entries 返回全部打新记录，附带派生字段。
func (s *Service) Entries(ctx context.Context) ([]ledger.View, error) {
	entries, err := s.store.ListEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取账本失败：%w", err)
	}
	views := make([]ledger.View, 0, len(entries))
	for _, e := range entries {
		views = append(views, e.Compute())
	}
	return views, nil
}

// Entry 按 id 取一条记录。
func (s *Service) Entry(ctx context.Context, id int64) (ledger.View, error) {
	entries, err := s.store.ListEntries(ctx)
	if err != nil {
		return ledger.View{}, fmt.Errorf("读取账本失败：%w", err)
	}
	for _, e := range entries {
		if e.ID == id {
			return e.Compute(), nil
		}
	}
	return ledger.View{}, fmt.Errorf("记录 %d 不存在", id)
}

// storePath 由 store 提供，用于界面展示数据位置。
func (s *Service) storePath() string { return s.store.Path() }

// StorePath 返回账本数据库路径。
func (s *Service) StorePath() string { return s.store.Path() }

// bump 记录一次数据改动。
func (s *Service) bump() {
	s.mu.Lock()
	s.rev++
	s.mu.Unlock()
}

// Revision 返回数据版本号。界面定时轮询它，变了就重新读取快照。
func (s *Service) Revision(ctx context.Context) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

// Refresh 从北交所同步新股列表。主数据源失败时自动切换备用源。
//
// scope 可为 "all"（默认）、"issuing"（仍在发行流程）、"done"（已上市）。
func (s *Service) Refresh(ctx context.Context, scope string) (RefreshResult, error) {
	if scope == "" {
		scope = string(source.ScopeAll)
	}
	res, err := s.sources.Fetch(ctx, source.Scope(scope))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("同步新股列表失败，请检查网络后重试：%w", err)
	}

	before, err := s.store.ListIssues(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	known := make(map[string]bool, len(before))
	for _, issue := range before {
		known[issue.FxCode] = true
	}
	if err := s.store.SaveIssues(ctx, res.Issues, res.Source); err != nil {
		return RefreshResult{}, fmt.Errorf("保存新股列表失败：%w", err)
	}

	added := 0
	for _, issue := range res.Issues {
		if !known[issue.FxCode] {
			added++
		}
	}

	meta := Meta{
		IssuesUpdatedAt: time.Now().Format(time.RFC3339),
		SourceLabel:     res.SourceLabel,
		Fallback:        res.Fallback,
		Warning:         res.Warning,
		DataPath:        s.storePath(),
	}
	s.mu.Lock()
	s.lastMeta = meta
	s.mu.Unlock()
	s.bump()

	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	return RefreshResult{
		Count:   len(res.Issues),
		Added:   added,
		Meta:    meta,
		Summary: snapshot.Summary,
	}, nil
}

// SaveEntry 新增或更新一条打新记录，返回保存后的视图。
//
// 账户名称与费率由服务端按券商账户补齐并快照到记录里：界面只需要给 brokerId，
// 之后即便改了账户的佣金率或利率，历史记录的成本也不变。
func (s *Service) SaveEntry(ctx context.Context, entry ledger.Entry) (ledger.View, error) {
	brokers, err := s.store.ListBrokers(ctx)
	if err != nil {
		return ledger.View{}, fmt.Errorf("读取券商账户失败：%w", err)
	}
	broker, ok := findBroker(brokers, entry.BrokerID)
	if !ok {
		return ledger.View{}, ledger.ErrMissingAccount
	}
	entry.Account = broker.Label()
	entry.AnnualRatePPM = broker.AnnualRatePPM
	entry.CommissionPPM = broker.CommissionPPM
	entry.MinCommissionFen = broker.MinCommissionFen
	if entry.PurchaseDate == "" {
		// 没带申购日时用新股缓存里的日期，冻款默认从申购日起算。
		if issues, err := s.store.ListIssues(ctx); err == nil {
			for _, issue := range issues {
				if issue.FxCode == entry.FxCode {
					entry.PurchaseDate = issue.PurchaseDate
					break
				}
			}
		}
	}

	saved, err := s.store.SaveEntry(ctx, entry)
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrDuplicateEntry):
			return ledger.View{}, fmt.Errorf("%s 在账户「%s」下已有记录，请直接修改那一条", entry.Name, entry.Account)
		case errors.Is(err, ledger.ErrMissingCode):
			return ledger.View{}, err
		case errors.Is(err, ledger.ErrMissingAccount):
			return ledger.View{}, err
		case errors.Is(err, ledger.ErrNegativeValue):
			return ledger.View{}, err
		case errors.Is(err, ledger.ErrNoAllotment):
			return ledger.View{}, err
		case errors.Is(err, ledger.ErrSoldOverAllotted):
			return ledger.View{}, err
		case errors.Is(err, ledger.ErrBadDate):
			return ledger.View{}, err
		}
		return ledger.View{}, fmt.Errorf("保存记录失败：%w", err)
	}
	s.bump()
	return saved.Compute(), nil
}

// DeleteEntry 删除一条记录。
func (s *Service) DeleteEntry(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("记录不存在")
	}
	if err := s.store.DeleteEntry(ctx, id); err != nil {
		return fmt.Errorf("删除记录失败：%w", err)
	}
	s.bump()
	return nil
}

// AppendSale 给一条记录追加一笔卖出，返回更新后的视图。
//
// agent 记「今天又卖了一笔」最常用这个：不必先把整条记录读出来再写回去，也就
// 不会在写回时把别处的改动覆盖掉。
func (s *Service) AppendSale(ctx context.Context, entryID int64, sale ledger.Sale) (ledger.View, error) {
	if err := sale.Validate(); err != nil {
		return ledger.View{}, err
	}
	view, err := s.Entry(ctx, entryID)
	if err != nil {
		return ledger.View{}, err
	}
	entry := view.Entry
	if len(entry.Sales) >= ledger.MaxSales {
		return ledger.View{}, ledger.ErrTooManySales
	}
	sale.ID = 0
	entry.Sales = append(entry.Sales, sale)
	return s.SaveEntry(ctx, entry)
}

// UpdateSale 用给定的内容替换某一笔卖出。
func (s *Service) UpdateSale(ctx context.Context, entryID, saleID int64, sale ledger.Sale) (ledger.View, error) {
	if err := sale.Validate(); err != nil {
		return ledger.View{}, err
	}
	sales, err := s.salesOf(ctx, entryID)
	if err != nil {
		return ledger.View{}, err
	}
	found := false
	for i := range sales {
		if sales[i].ID == saleID {
			sale.ID = saleID
			sales[i], found = sale, true
			break
		}
	}
	if !found {
		return ledger.View{}, fmt.Errorf("这笔卖出不存在或已被删除")
	}
	return s.saveSales(ctx, entryID, sales)
}

// RemoveSale 删除某一笔卖出。
func (s *Service) RemoveSale(ctx context.Context, entryID, saleID int64) (ledger.View, error) {
	sales, err := s.salesOf(ctx, entryID)
	if err != nil {
		return ledger.View{}, err
	}
	kept := make([]ledger.Sale, 0, len(sales))
	for _, sale := range sales {
		if sale.ID != saleID {
			kept = append(kept, sale)
		}
	}
	if len(kept) == len(sales) {
		return ledger.View{}, fmt.Errorf("这笔卖出不存在或已被删除")
	}
	return s.saveSales(ctx, entryID, kept)
}

// salesOf 取出一条记录当前的卖出明细。
func (s *Service) salesOf(ctx context.Context, entryID int64) ([]ledger.Sale, error) {
	view, err := s.Entry(ctx, entryID)
	if err != nil {
		return nil, err
	}
	return view.Sales, nil
}

// saveSales 用给定明细替换整条记录的卖出后保存。
func (s *Service) saveSales(ctx context.Context, entryID int64, sales []ledger.Sale) (ledger.View, error) {
	view, err := s.Entry(ctx, entryID)
	if err != nil {
		return ledger.View{}, err
	}
	entry := view.Entry
	entry.Sales = sales
	return s.SaveEntry(ctx, entry)
}

// SaveBroker 新增或更新一个券商账户。
func (s *Service) SaveBroker(ctx context.Context, broker ledger.Broker) (ledger.Broker, error) {
	saved, err := s.store.SaveBroker(ctx, broker)
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrMissingBroker):
			return ledger.Broker{}, errors.New("请填写券商名称")
		case errors.Is(err, ledger.ErrRateRange):
			return ledger.Broker{}, errors.New("费率不能为负，也不能超过 100%")
		}
		return ledger.Broker{}, err
	}
	s.bump()
	return saved, nil
}

// DeleteBroker 删除券商账户。账户下还有记录时会被拒绝。
func (s *Service) DeleteBroker(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("券商账户不存在")
	}
	if err := s.store.DeleteBroker(ctx, id); err != nil {
		return err
	}
	s.bump()
	return nil
}

// findBroker 在账户列表里查找指定 ID。
func findBroker(brokers []ledger.Broker, id int64) (ledger.Broker, bool) {
	for _, b := range brokers {
		if b.ID == id {
			return b, true
		}
	}
	return ledger.Broker{}, false
}

// ExportJSON 返回账本备份的 JSON 文本。
//
// 桌面版通过文件对话框写入磁盘（见 main 包），网页版可直接下载这份文本。
func (s *Service) ExportJSON(ctx context.Context) (string, error) {
	backup, err := s.store.Export(ctx)
	if err != nil {
		return "", fmt.Errorf("导出备份失败：%w", err)
	}
	payload, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// ImportJSON 用备份文本替换账本记录，返回导入条数。
func (s *Service) ImportJSON(ctx context.Context, payload string) (int, error) {
	if payload == "" {
		return 0, errors.New("备份内容为空")
	}
	count, err := s.store.Import(ctx, []byte(payload))
	if err != nil {
		if errors.Is(err, store.ErrInvalidBackup) {
			return 0, errors.New("这不是有效的账本备份文件")
		}
		return 0, fmt.Errorf("导入备份失败：%w", err)
	}
	s.bump()
	return count, nil
}
