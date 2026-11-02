package ledger

import (
	"fmt"
	"sort"
)

// StatsFilter 是收益统计的筛选条件，零值表示不筛选。
//
// 时间按「申购日」归属：一只新股从申购到卖出可能跨月，但它是同一笔打新，
// 统计时整个批次算在申购当月，这样每个月、每年的数字加起来正好等于总计，
// 不会因为卖出跨月而在两个区间里各算一半。
type StatsFilter struct {
	// BrokerID 只看某个券商账户，0 表示全部账户。
	BrokerID int64 `json:"brokerId"`
	// Year 按申购日筛选年份，0 表示全部年份。
	Year int `json:"year"`
	// Month 按申购月筛选（1-12），0 表示整年。
	Month int `json:"month"`
}

// Filtering 报告这个条件是否真的在筛选。
func (f StatsFilter) Filtering() bool {
	return f.BrokerID != 0 || f.Year != 0 || f.Month != 0
}

// Valid 检查条件取值是否合理。
func (f StatsFilter) Valid() error {
	if f.BrokerID < 0 {
		return fmt.Errorf("券商账户编号不能为负")
	}
	if f.Year != 0 && (f.Year < 1990 || f.Year > 2200) {
		return fmt.Errorf("年份需要在 1990 到 2200 之间")
	}
	if f.Month != 0 && (f.Month < 1 || f.Month > 12) {
		return fmt.Errorf("月份需要在 1 到 12 之间")
	}
	return nil
}

// Match 报告一条记录是否落在筛选范围内。
//
// 没填申购日的记录进不了按年、按月的统计——猜一个日期会让汇总对不上账，
// 它们仍然计入「全部」视图。
func (f StatsFilter) Match(e Entry) bool {
	if f.BrokerID != 0 && e.BrokerID != f.BrokerID {
		return false
	}
	if f.Year == 0 && f.Month == 0 {
		return true
	}
	year, month, ok := entryMonth(e)
	if !ok {
		return false
	}
	if f.Year != 0 && year != f.Year {
		return false
	}
	if f.Month != 0 && month != f.Month {
		return false
	}
	return true
}

// entryMonth 取出记录申购日的年月。
func entryMonth(e Entry) (year, month int, ok bool) {
	day, err := parseDate(e.PurchaseDate)
	if err != nil {
		return 0, 0, false
	}
	return day.Year(), int(day.Month()), true
}

// Bucket 是按年份或月份分组的统计。
//
// 字段与 Summary 同口径：金额一律是分，收益率是万分之一，方便和总计逐项对照。
type Bucket struct {
	// Key 是排序与比较用的键，如 "2026"、"2026-10"；没填申购日的记录为 ""。
	Key string `json:"key"`
	// Label 是给人看的名字，如「2026 年」「2026 年 10 月」。
	Label string `json:"label"`
	// EntryCount 是本组的记录数。
	EntryCount int `json:"entryCount"`
	// AppliedIssues 是申购过的新股只数（去重）。
	AppliedIssues int `json:"appliedIssues"`
	// AllottedIssues 是中签过的新股只数（去重）。
	AllottedIssues int `json:"allottedIssues"`
	// AllottedShares 是中签总股数。
	AllottedShares int64 `json:"allottedShares"`
	// ApplyFundsFen 是申购占用资金合计。
	ApplyFundsFen int64 `json:"applyFundsFen"`
	// SaleCount 是卖出笔数合计。
	SaleCount int `json:"saleCount"`
	// SellAmountFen 是卖出成交金额合计。
	SellAmountFen int64 `json:"sellAmountFen"`
	// SellFeeFen 是卖出费用合计。
	SellFeeFen int64 `json:"sellFeeFen"`
	// OtherFeeFen 是其他费用合计。
	OtherFeeFen int64 `json:"otherFeeFen"`
	// CapitalCostFen 是资金成本合计。
	CapitalCostFen int64 `json:"capitalCostFen"`
	// RealizedFen 是已实现净收益合计。
	RealizedFen int64 `json:"realizedFen"`
	// HoldingShares 是仍未卖出的股数。
	HoldingShares int64 `json:"holdingShares"`
	// ReturnBps 是本组的已实现收益率。
	ReturnBps int64 `json:"returnBps"`
	// Undated 表示本组是「没填申购日」的记录。
	Undated bool `json:"undated,omitempty"`
}

// StatsEntry 是明细里的一行：一条打新记录在统计口径下的数字。
//
// 它与 Bucket、Summary 同字段同口径（金额是分，收益率是万分之一），
// 所以把一组的明细逐行相加，正好等于该组的合计——界面上的「这个月赚了多少」
// 能落到具体是哪几只票赚的，且对得上账。
type StatsEntry struct {
	// EntryID 是记录编号，界面据此打开对应记录。
	EntryID int64 `json:"entryId"`
	// BrokerID 与 Account 是记录所属的券商账户。
	BrokerID int64  `json:"brokerId"`
	Account  string `json:"account"`
	// FxCode、StockCode、Name 是新股的申购代码、证券代码与名称。
	FxCode    string `json:"fxCode"`
	StockCode string `json:"stockCode"`
	Name      string `json:"name"`
	// PurchaseDate 是申购日，也是统计归属的日期。
	PurchaseDate string `json:"purchaseDate"`
	// AllottedShares 是中签股数，0 表示未中签。
	AllottedShares int64 `json:"allottedShares"`
	// ApplyFundsFen 是这笔占用的申购资金。
	ApplyFundsFen int64 `json:"applyFundsFen"`
	// SaleCount 是卖出笔数，SellAmountFen 是卖出成交金额。
	SaleCount     int   `json:"saleCount"`
	SellAmountFen int64 `json:"sellAmountFen"`
	// CapitalCostFen 是这笔的资金成本。
	CapitalCostFen int64 `json:"capitalCostFen"`
	// RealizedFen 是这笔的已实现净收益，ReturnBps 是它的收益率。
	RealizedFen int64 `json:"realizedFen"`
	ReturnBps   int64 `json:"returnBps"`
	// HoldingShares 是仍未卖出的中签股数。
	HoldingShares int64 `json:"holdingShares"`
	// Status 是给用户看的中文状态。
	Status string `json:"status"`
}

// Stats 是一份带筛选条件的收益统计。
type Stats struct {
	// Filter 是本份统计采用的筛选条件，原样带回给调用方。
	Filter StatsFilter `json:"filter"`
	// Summary 是筛选后记录的汇总。
	Summary Summary `json:"summary"`
	// Details 是筛选后记录的逐条明细，按申购日从新到旧、同日按代码倒序，
	// 没填申购日的垫底——与「我的打新」列表同一个顺序。
	//
	// 它只受筛选条件影响，不受分组影响：点某个月看细节时，界面要的就是这个月
	// 落到了哪几条记录上，而不是再按某个维度分一次组。
	Details []StatsEntry `json:"details"`
	// ByYear 是按申购年份分组的统计，最近的年份在前。
	ByYear []Bucket `json:"byYear"`
	// ByMonth 是按申购月份分组的统计，最近的月份在前。
	//
	// 它只受账户筛选影响，年份与月份都不参与：先在月度分布里点某个月，再看
	// 别的年份那个月，是同一个动作。被年份筛掉之后整片消失，想跳回去只能先
	// 重置筛选，等于把「按月份浏览」的路堵死了。
	ByMonth []Bucket `json:"byMonth"`
	// Years 是账本里出现过的申购年份，从新到旧，供界面生成年份下拉框。
	Years []int `json:"years"`
	// Accounts 是账本里用到的券商账户名，供界面生成账户下拉框。
	Accounts []StatsAccount `json:"accounts"`
}

// StatsAccount 是筛选用的券商账户条目。
type StatsAccount struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// BuildStats 汇总记录：先按条件筛出记录，再给出总计与按年、按月的拆分。
func BuildStats(entries []Entry, filter StatsFilter) Stats {
	out := Stats{
		Filter:   filter,
		ByYear:   []Bucket{},
		ByMonth:  []Bucket{},
		Details:  []StatsEntry{},
		Years:    []int{},
		Accounts: []StatsAccount{},
	}

	years := make(map[int]bool)
	accounts := make(map[int64]StatsAccount)
	for _, e := range entries {
		if year, _, ok := entryMonth(e); ok {
			years[year] = true
		}
		if _, ok := accounts[e.BrokerID]; !ok && e.BrokerID != 0 {
			accounts[e.BrokerID] = StatsAccount{ID: e.BrokerID, Label: e.Account}
		}
	}

	// 年份与账户下拉框列的是账本里的全集，不受当前筛选影响，
	// 否则选完一年就再也切不回别的年份。
	for year := range years {
		out.Years = append(out.Years, year)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out.Years)))
	for _, acc := range accounts {
		out.Accounts = append(out.Accounts, acc)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Label < out.Accounts[j].Label })

	kept := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if filter.Match(e) {
			kept = append(kept, e)
		}
	}
	out.Summary = Summarize(kept)
	out.Details = detailRows(kept)

	out.ByYear = groupBuckets(entries, filter, byYear)
	// 月度分布只跟账户走：年份与月份都不筛。点了某个月就只剩那一年，
	// 别的年份的月份随即消失，想换一年看又得先重置——那份列表本来就是
	// 用来按月翻账本的，得一直看得到全部历史。
	monthFilter := StatsFilter{BrokerID: filter.BrokerID}
	out.ByMonth = groupBuckets(entries, monthFilter, byMonth)
	return out
}

// detailRows 把记录转成明细行，并按「我的打新」列表的顺序排：
// 申购日从新到旧，同日按申购代码倒序，没填申购日的垫底。
//
// 顺序在这里定死，不依赖调用方传进来的顺序：明细要能一眼对照列表，
// 也要保证同一份账本每次算出来的行序一致。
func detailRows(entries []Entry) []StatsEntry {
	rows := make([]StatsEntry, 0, len(entries))
	for _, e := range entries {
		v := e.Compute()
		rows = append(rows, StatsEntry{
			EntryID:        e.ID,
			BrokerID:       e.BrokerID,
			Account:        e.Account,
			FxCode:         e.FxCode,
			StockCode:      e.StockCode,
			Name:           e.Name,
			PurchaseDate:   e.PurchaseDate,
			AllottedShares: e.AllottedShares,
			ApplyFundsFen:  v.ApplyFundsFen,
			SaleCount:      v.SaleCount,
			SellAmountFen:  v.SellAmountFen,
			CapitalCostFen: v.CapitalCostFen,
			RealizedFen:    v.RealizedFen,
			ReturnBps:      v.ReturnBps,
			HoldingShares:  v.HoldingShares,
			Status:         v.Status,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		di, dj := rows[i].PurchaseDate, rows[j].PurchaseDate
		if di != dj {
			if di == "" {
				return false
			}
			if dj == "" {
				return true
			}
			return di > dj
		}
		if rows[i].FxCode != rows[j].FxCode {
			return rows[i].FxCode > rows[j].FxCode
		}
		return rows[i].EntryID > rows[j].EntryID
	})
	return rows
}

// byYear、byMonth 是两种分组方式，按 months 的粒度拆同一批记录。
type grouping int

const (
	byYear grouping = iota
	byMonth
)

// groupBuckets 按年或按月把记录分组求和。
func groupBuckets(entries []Entry, filter StatsFilter, mode grouping) []Bucket {
	groups := make(map[string]*bucketAcc)
	order := []string{}
	for _, e := range entries {
		if !filter.Match(e) {
			continue
		}
		key, label, undated := bucketKey(e, mode)
		acc, ok := groups[key]
		if !ok {
			acc = newBucketAcc(key, label, undated)
			groups[key] = acc
			order = append(order, key)
		}
		acc.add(e, e.Compute())
	}

	buckets := make([]Bucket, 0, len(order))
	for _, key := range order {
		buckets = append(buckets, groups[key].finish())
	}
	// 最近的在前，没填申购日的垫底。
	sort.SliceStable(buckets, func(i, j int) bool {
		if buckets[i].Undated != buckets[j].Undated {
			return !buckets[i].Undated
		}
		return buckets[i].Key > buckets[j].Key
	})
	return buckets
}

// bucketKey 返回一条记录所属分组的键与名字。
func bucketKey(e Entry, mode grouping) (key, label string, undated bool) {
	year, month, ok := entryMonth(e)
	if !ok {
		return "", "未填申购日", true
	}
	if mode == byYear {
		return fmt.Sprintf("%04d", year), fmt.Sprintf("%d 年", year), false
	}
	return fmt.Sprintf("%04d-%02d", year, month), fmt.Sprintf("%d 年 %d 月", year, month), false
}

// bucketAcc 累加一组记录，最后算出收益率。
type bucketAcc struct {
	bucket   Bucket
	applied  map[string]bool
	allotted map[string]bool
	// base 是收益率的分母：卖出部分的缴款成本 + 全部支出。
	base int64
}

func newBucketAcc(key, label string, undated bool) *bucketAcc {
	return &bucketAcc{
		bucket:   Bucket{Key: key, Label: label, Undated: undated},
		applied:  make(map[string]bool),
		allotted: make(map[string]bool),
	}
}

func (a *bucketAcc) add(e Entry, v View) {
	b := &a.bucket
	b.EntryCount++
	if e.ApplyShares > 0 || e.AllottedShares > 0 {
		a.applied[e.FxCode] = true
	}
	if e.AllottedShares > 0 {
		a.allotted[e.FxCode] = true
	}
	b.AllottedShares += e.AllottedShares
	b.ApplyFundsFen += v.ApplyFundsFen
	b.SaleCount += v.SaleCount
	b.SellAmountFen += v.SellAmountFen
	b.SellFeeFen += v.SellFeeFen
	b.OtherFeeFen += e.OtherFeeFen
	b.CapitalCostFen += v.CapitalCostFen
	b.RealizedFen += v.RealizedFen
	b.HoldingShares += v.HoldingShares
	if v.SoldShares > 0 {
		a.base += v.SoldShares*e.allotPrice() + v.TotalCostFen
	}
}

func (a *bucketAcc) finish() Bucket {
	a.bucket.AppliedIssues = len(a.applied)
	a.bucket.AllottedIssues = len(a.allotted)
	if a.base > 0 {
		a.bucket.ReturnBps = a.bucket.RealizedFen * 10000 / a.base
	}
	return a.bucket
}
