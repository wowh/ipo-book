// Package ledger 定义打新账本的领域模型与收益核算。
//
// 所有金额都是整数：单价以「分」为单位存储，金额由「股数 × 单价分」得出，
// 同样是分。费率不用百分数小数，而是以百万分之一为单位（ppm）的整数：
// 年化 3.85% = 38500，佣金万分之 2.5 = 250。利率、佣金、资金成本全程整数运算，
// 只在最终结果上做一次四舍五入，避免 0.1+0.2 一类的误差累积到收益上。
package ledger

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// rateScale 是费率的定点倍数：1 ppm = 百万分之一。
const rateScale = 1_000_000

// defaultFreezeDays 是没有填写退款日时按几个自然日估算资金占用。
//
// 北交所申购资金在申购日冻结，T+2 前后退回，用 2 天做保守估算；填了实际
// 退款日就以实际天数为准。估算结果在界面上会标注出来，不与实际成本混同。
const defaultFreezeDays = 2

// MaxSales 是一条记录允许的卖出笔数上限。中签量再大也不会卖这么多次，
// 超过这个数几乎肯定是数据出错（例如脚本重复追加），直接拒绝。
const MaxSales = 200

// Broker 是一个券商账户：同一家券商可以有多个账户，按账户分别核算。
type Broker struct {
	ID int64 `json:"id"`
	// Name 是券商名称，如「华泰证券」。
	Name string `json:"name"`
	// Account 是账户别称，如「主账户」。
	Account string `json:"account"`
	// CommissionPPM 是卖出佣金率，单位百万分之一（万分之 2.5 = 250）。
	CommissionPPM int64 `json:"commissionPpm"`
	// MinCommissionFen 是单笔最低佣金，单位分。
	MinCommissionFen int64 `json:"minCommissionFen"`
	// AnnualRatePPM 是资金年化利率，单位百万分之一（3.85% = 38500），
	// 用于估算冻结资金的占用成本。
	AnnualRatePPM int64  `json:"annualRatePpm"`
	Note          string `json:"note"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// Label 返回界面上使用的账户全称。
func (b Broker) Label() string {
	name := strings.TrimSpace(b.Name)
	account := strings.TrimSpace(b.Account)
	switch {
	case name == "" && account == "":
		return "未命名账户"
	case account == "":
		return name
	case name == "":
		return account
	case name == account:
		return name
	default:
		return name + " · " + account
	}
}

// 券商账户的领域错误。
var (
	// ErrMissingBroker 表示缺少券商名称。
	ErrMissingBroker = errors.New("请填写券商名称")
	// ErrRateRange 表示费率超出允许范围。
	ErrRateRange = errors.New("费率不能为负，也不能超过 100%")
)

// Validate 检查券商账户字段。
func (b *Broker) Validate() error {
	b.Name = strings.TrimSpace(b.Name)
	b.Account = strings.TrimSpace(b.Account)
	b.Note = strings.TrimSpace(b.Note)
	if b.Name == "" {
		return ErrMissingBroker
	}
	if b.Account == "" {
		b.Account = "默认账户"
	}
	if len([]rune(b.Name)) > 40 || len([]rune(b.Account)) > 40 || len([]rune(b.Note)) > 200 {
		return errors.New("券商名称、账户或备注过长")
	}
	for _, rate := range []int64{b.CommissionPPM, b.AnnualRatePPM} {
		if rate < 0 || rate > 100*rateScale/100 {
			return ErrRateRange
		}
	}
	if b.MinCommissionFen < 0 {
		return errors.New("最低佣金不能为负")
	}
	return nil
}

// Sale 是一笔卖出成交。
//
// 一只新股常常分几笔卖：开盘冲高卖一半，回落再卖剩下的；额度大的还会连着
// 几天出。每笔有自己的成交日、价格、股数与费用，合计值一律由明细加出来，
// 不单独存一个「一共卖了多少股」——只存合计就再也拆不回去了。
type Sale struct {
	// ID 是这笔成交的标识。新增时留空，读出时由存储层填上。
	ID int64 `json:"id,omitempty"`
	// Date 是成交日，YYYY-MM-DD。
	Date string `json:"date"`
	// Shares 是成交数量（股）。
	Shares int64 `json:"shares"`
	// PriceFen 是成交价，单位分。
	PriceFen int64 `json:"priceFen"`
	// AmountFen 是成交金额，单位分。为 0 时按「股数 × 成交价」算。
	//
	// 交割单给的是金额：300 股成交 8213.00 元，均价 27.3767 元除不尽。
	// 填了金额就以它为准，成交价只用于展示。
	AmountFen int64 `json:"amountFen"`
	// FeeFen 是这笔的费用，单位分。为 0 时按账户佣金率估算，
	// 并且每笔单独遵守最低佣金——券商就是这么收的。
	FeeFen int64  `json:"feeFen"`
	Note   string `json:"note"`
}

// Amount 返回这笔的成交金额：填了金额用金额，否则股数 × 成交价。
func (s Sale) Amount() int64 {
	if s.AmountFen > 0 {
		return s.AmountFen
	}
	return s.Shares * s.PriceFen
}

// AveragePriceFen 返回这笔的成交均价，仅用于展示。
func (s Sale) AveragePriceFen() int64 {
	if s.Shares <= 0 {
		return 0
	}
	return divRound(s.Amount(), s.Shares)
}

// Validate 检查一笔成交是否合理，并补上默认成交日。
func (s *Sale) Validate() error {
	s.Date = strings.TrimSpace(s.Date)
	s.Note = strings.TrimSpace(s.Note)
	if s.Shares <= 0 {
		return ErrSaleShares
	}
	if s.PriceFen < 0 || s.AmountFen < 0 || s.FeeFen < 0 {
		return ErrNegativeValue
	}
	if s.AmountFen == 0 && s.PriceFen == 0 {
		return ErrSalePrice
	}
	if s.Date == "" {
		s.Date = time.Now().Format(time.DateOnly)
	}
	if _, err := parseDate(s.Date); err != nil {
		return ErrBadDate
	}
	if len([]rune(s.Note)) > 200 {
		return errors.New("卖出备注过长")
	}
	return nil
}

// Entry 是一条打新记录：某账户对某只新股的一次申购及其后续处置。
type Entry struct {
	ID int64 `json:"id"`
	// BrokerID 指向券商账户，一只新股在同一个账户下只允许一条记录。
	BrokerID int64 `json:"brokerId"`
	// Account 是录入时的账户名称快照，便于展示与历史留痕。
	Account   string `json:"account"`
	FxCode    string `json:"fxCode"`
	StockCode string `json:"stockCode"`
	Name      string `json:"name"`
	// IssuePriceFen 是申购时的发行价快照，单位分。后续行情变化不影响它。
	IssuePriceFen int64 `json:"issuePriceFen"`
	// PurchaseDate 是申购日，YYYY-MM-DD，也是资金冻结的默认起始日。
	PurchaseDate string `json:"purchaseDate"`
	// ApplyShares 是申购数量（股）。
	ApplyShares int64 `json:"applyShares"`
	// AllottedShares 是中签数量（股），0 表示未中签或尚未公布。
	AllottedShares int64 `json:"allottedShares"`
	// AllotPriceFen 是中签缴款价，单位分，默认等于发行价。
	AllotPriceFen int64 `json:"allotPriceFen"`
	// FreezeDate 是资金冻结起始日，留空时取申购日。
	FreezeDate string `json:"freezeDate"`
	// RefundDate 是资金退回日，留空时按默认天数估算占用时间。
	RefundDate string `json:"refundDate"`
	// AnnualRatePPM 是估算资金成本用的年化利率快照，来自券商账户。
	AnnualRatePPM int64 `json:"annualRatePpm"`
	// CommissionPPM 与 MinCommissionFen 是卖出佣金的计算参数快照。
	CommissionPPM    int64 `json:"commissionPpm"`
	MinCommissionFen int64 `json:"minCommissionFen"`
	// OtherFeeFen 是卖出以外的一次性费用，如中签手续费，单位分。
	OtherFeeFen int64 `json:"otherFeeFen"`
	// CapitalCostFen 是实际资金成本，单位分。为 0 表示按年化利率估算。
	CapitalCostFen int64 `json:"capitalCostFen"`
	// Sales 是卖出明细，一笔一条。
	//
	// 卖出股数、成交金额、费用、最后一笔日期都由它加出来（见 View），写入时
	// 也只认它。没有明细就表示仍持有。
	Sales     []Sale `json:"sales"`
	Note      string `json:"note"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// 领域错误。界面据此给出可操作的提示，而不是透传底层信息。
var (
	// ErrMissingCode 表示缺少申购代码。
	ErrMissingCode = errors.New("请填写新股代码")
	// ErrMissingAccount 表示缺少账户。
	ErrMissingAccount = errors.New("请选择一个券商账户")
	// ErrNegativeValue 表示数量或金额为负。
	ErrNegativeValue = errors.New("数量与金额不能为负数")
	// ErrSoldOverAllotted 表示卖出数量超过中签数量。
	ErrSoldOverAllotted = errors.New("卖出数量不能超过中签数量")
	// ErrNoAllotment 表示没有中签却填写了卖出。
	ErrNoAllotment = errors.New("未中签的记录不能填写卖出")
	// ErrSaleShares 表示一笔卖出的股数不是正数。
	ErrSaleShares = errors.New("每笔卖出的股数要大于 0")
	// ErrSalePrice 表示一笔卖出既没有成交价也没有成交金额。
	ErrSalePrice = errors.New("每笔卖出要填成交价或成交金额")
	// ErrTooManySales 表示卖出笔数多得不正常，多半是数据出错。
	ErrTooManySales = errors.New("卖出笔数过多，请检查数据")
	// ErrDuplicateEntry 表示同一账户重复录入同一只新股。
	ErrDuplicateEntry = errors.New("该账户已有这只新股的记录")
	// ErrBadDate 表示日期格式或先后顺序不对。
	ErrBadDate = errors.New("日期须为 1990 至 2200 年之间的 YYYY-MM-DD，且退款日不能早于冻结日")
)

// Validate 检查记录的内部一致性。它只做与账务有关的基本校验，
// 唯一性由存储层在事务中保证。
func (e *Entry) Validate() error {
	e.FxCode = strings.TrimSpace(e.FxCode)
	e.Account = strings.TrimSpace(e.Account)
	if e.FxCode == "" {
		return ErrMissingCode
	}
	if e.BrokerID <= 0 {
		return ErrMissingAccount
	}
	numbers := []int64{
		e.IssuePriceFen, e.ApplyShares, e.AllottedShares,
		e.AllotPriceFen, e.OtherFeeFen, e.CapitalCostFen,
		e.AnnualRatePPM, e.CommissionPPM, e.MinCommissionFen,
	}
	for _, n := range numbers {
		if n < 0 {
			return ErrNegativeValue
		}
	}
	if len(e.Sales) > MaxSales {
		return ErrTooManySales
	}
	var soldShares int64
	for i := range e.Sales {
		if err := e.Sales[i].Validate(); err != nil {
			// 指明是第几笔，否则用户面对十几笔明细无从下手。
			return fmt.Errorf("第 %d 笔卖出：%w", i+1, err)
		}
		soldShares += e.Sales[i].Shares
	}
	if soldShares > 0 && e.AllottedShares == 0 {
		return ErrNoAllotment
	}
	if soldShares > e.AllottedShares {
		return ErrSoldOverAllotted
	}
	if e.AllotPriceFen == 0 {
		e.AllotPriceFen = e.IssuePriceFen
	}
	if e.FreezeDate == "" {
		e.FreezeDate = e.PurchaseDate
	}
	if _, err := parseDate(e.FreezeDate); err != nil && e.FreezeDate != "" {
		return ErrBadDate
	}
	if e.RefundDate != "" {
		if _, err := parseDate(e.RefundDate); err != nil {
			return ErrBadDate
		}
		if e.FreezeDate != "" && e.RefundDate < e.FreezeDate {
			return ErrBadDate
		}
	}
	return nil
}

// parseDate 解析 YYYY-MM-DD，并拒绝明显不合理的年份。
func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, err
	}
	if t.Year() < 1990 || t.Year() > 2200 {
		return time.Time{}, ErrBadDate
	}
	return t, nil
}

// View 是带派生字段的记录，供界面展示。
type View struct {
	Entry
	// ApplyFundsFen 是申购占用的资金（发行价 × 申购股数）。
	ApplyFundsFen int64 `json:"applyFundsFen"`
	// FreezeDays 是资金占用天数，用于估算资金成本。
	FreezeDays int64 `json:"freezeDays"`
	// FreezeEstimated 表示占用天数按默认值估算，未填写退款日。
	FreezeEstimated bool `json:"freezeEstimated"`
	// CostFen 是中签部分的缴款成本（不含费用）。
	CostFen int64 `json:"costFen"`
	// SoldShares 是卖出明细的股数合计。
	SoldShares int64 `json:"soldShares"`
	// SellAmountFen 是卖出成交金额合计。
	SellAmountFen int64 `json:"sellAmountFen"`
	// SellPriceFen 是按成交金额与股数回算的均价，仅用于展示。
	SellPriceFen int64 `json:"sellPriceFen"`
	// SellDate 是最后一笔卖出的日期。
	SellDate string `json:"sellDate"`
	// SaleCount 是卖出笔数。
	SaleCount int `json:"saleCount"`
	// SellFeeFen 是实际采用的卖出费用合计，可能是按佣金率估算的结果。
	SellFeeFen int64 `json:"sellFeeFen"`
	// FeeEstimated 表示有卖出费用是按券商佣金率估算的。
	FeeEstimated bool `json:"feeEstimated"`
	// CapitalCostFen 是实际采用的资金成本。
	CapitalCostFen int64 `json:"capitalCostFen"`
	// CapitalCostEstimated 表示资金成本按年化利率估算。
	CapitalCostEstimated bool `json:"capitalCostEstimated"`
	// TotalCostFen 是这笔打新的全部支出：卖出费用 + 其他费用 + 资金成本。
	TotalCostFen int64 `json:"totalCostFen"`
	// RealizedFen 是已实现净收益：卖出收入 − 卖出部分成本 − 全部支出。
	RealizedFen int64 `json:"realizedFen"`
	// HoldingShares 是尚未卖出的中签股数。
	HoldingShares int64 `json:"holdingShares"`
	// HoldingCostFen 是未卖出部分占用的成本。
	HoldingCostFen int64 `json:"holdingCostFen"`
	// ReturnBps 是已实现收益率，单位为万分之一（basis point）。
	ReturnBps int64 `json:"returnBps"`
	// Status 是给用户看的中文状态。
	Status string `json:"status"`
}

// 记录状态。
const (
	StatusApplied   = "已申购"
	StatusAllotted  = "已中签"
	StatusNoAllot   = "未中签"
	StatusPartSold  = "部分卖出"
	StatusSold      = "已卖出"
	StatusPendingIn = "待公布"
)

// allotPrice 返回缴款价，未单独填写时沿用发行价。
func (e Entry) allotPrice() int64 {
	if e.AllotPriceFen > 0 {
		return e.AllotPriceFen
	}
	return e.IssuePriceFen
}

// freezeDays 返回资金占用天数。填了退款日按实际天数算，否则按默认天数。
func (e Entry) freezeDays() (days int64, estimated bool) {
	start := e.FreezeDate
	if start == "" {
		start = e.PurchaseDate
	}
	if start == "" || e.RefundDate == "" {
		return defaultFreezeDays, true
	}
	from, err := parseDate(start)
	if err != nil {
		return defaultFreezeDays, true
	}
	to, err := parseDate(e.RefundDate)
	if err != nil || to.Before(from) {
		return defaultFreezeDays, true
	}
	return int64(to.Sub(from).Hours() / 24), false
}

// interest 估算冻结资金的占用成本：本金 × 年化利率 × 天数 ÷ 365。
//
// 用 big.Int 承接中间乘积，只在最后做一次四舍五入，避免大额资金溢出或分段
// 舍入带来的偏差。
func interest(principalFen, ratePPM, days int64) int64 {
	if principalFen <= 0 || ratePPM <= 0 || days <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(principalFen), big.NewInt(ratePPM))
	n.Mul(n, big.NewInt(days))
	d := big.NewInt(rateScale * 365)
	n.Add(n, new(big.Int).Quo(new(big.Int).Set(d), big.NewInt(2)))
	n.Quo(n, d)
	if !n.IsInt64() {
		return 0
	}
	return n.Int64()
}

// commission 按佣金率估算卖出费用，并遵守券商的最低佣金。
func commission(revenueFen, ratePPM, minFen int64) int64 {
	if revenueFen <= 0 || ratePPM <= 0 {
		return minFen
	}
	n := new(big.Int).Mul(big.NewInt(revenueFen), big.NewInt(ratePPM))
	n.Add(n, big.NewInt(rateScale/2))
	n.Quo(n, big.NewInt(rateScale))
	fee := int64(0)
	if n.IsInt64() {
		fee = n.Int64()
	}
	if fee < minFen {
		fee = minFen
	}
	return fee
}

// saleTotals 是卖出明细的合计。
type saleTotals struct {
	// count 是笔数。
	count int
	// shares 是成交股数合计。
	shares int64
	// amount 是成交金额合计。
	amount int64
	// fee 是费用合计，包含按佣金率估算的部分。
	fee int64
	// feeEstimated 表示有费用是估算出来的。
	feeEstimated bool
	// lastDate 是最后一笔的成交日。
	lastDate string
}

// averagePrice 返回回算的均价，仅用于展示。
func (t saleTotals) averagePrice() int64 {
	if t.shares <= 0 {
		return 0
	}
	return divRound(t.amount, t.shares)
}

// totals 汇总卖出明细：股数与成交金额直接相加，费用没填的按该笔估算。
func (e Entry) totals() saleTotals {
	var t saleTotals
	t.count = len(e.Sales)
	for _, s := range e.Sales {
		if s.Shares <= 0 {
			continue
		}
		if s.Date > t.lastDate {
			t.lastDate = s.Date
		}
		amount := s.Amount()
		t.shares += s.Shares
		t.amount += amount
		if s.FeeFen > 0 {
			t.fee += s.FeeFen
			continue
		}
		// 每笔单独算佣金并各自遵守最低佣金，与券商按笔收费的方式一致：
		// 分三笔卖出会比一次卖完多掏两次保底佣金，这本来就该算进成本。
		if fee := commission(amount, e.CommissionPPM, e.MinCommissionFen); fee > 0 {
			t.fee += fee
			t.feeEstimated = true
		}
	}
	return t
}

// divRound 做四舍五入的整除，负数按绝对值处理。
func divRound(v, d int64) int64 {
	if d == 0 {
		return 0
	}
	if v < 0 {
		return -((-v + d/2) / d)
	}
	return (v + d/2) / d
}

// withSaleAmounts 返回带完整成交金额的副本。
//
// 明细里没填金额时补上「股数 × 成交价」，读的人不必自己再乘一遍。只在展示与
// 计算时补，记录本身不动：存储里仍然只存用户真正填过的内容，界面上也就能
// 区分「用户填的金额」和「按价格推出来的金额」。
func (e Entry) withSaleAmounts() Entry {
	if len(e.Sales) == 0 {
		// 没卖出时也给空数组：读的人（尤其是 agent）不必再判 null。
		e.Sales = []Sale{}
		return e
	}
	sales := make([]Sale, len(e.Sales))
	copy(sales, e.Sales)
	for i := range sales {
		if sales[i].AmountFen == 0 {
			sales[i].AmountFen = sales[i].Amount()
		}
	}
	e.Sales = sales
	return e
}

// Compute 计算派生字段。
func (e Entry) Compute() View {
	e = e.withSaleAmounts()
	allotPrice := e.allotPrice()
	costFen := e.AllottedShares * allotPrice
	applyFunds := e.IssuePriceFen * e.ApplyShares
	totals := e.totals()
	holdingShares := e.AllottedShares - totals.shares
	if holdingShares < 0 {
		holdingShares = 0
	}
	holdingCostFen := holdingShares * allotPrice

	days, freezeEstimated := e.freezeDays()
	// 资金成本与卖出费用都可以按券商参数估算，用户填了实际值就以实际值为准。
	capitalCost := e.CapitalCostFen
	capitalEstimated := false
	if capitalCost == 0 {
		capitalCost = interest(applyFunds, e.AnnualRatePPM, days)
		capitalEstimated = capitalCost > 0
	}

	income := totals.amount
	sellFee := totals.fee
	totalCost := sellFee + e.OtherFeeFen + capitalCost

	var realizedFen int64
	if totals.shares > 0 {
		soldCost := totals.shares * allotPrice
		realizedFen = income - soldCost - totalCost
	}

	var returnBps int64
	if totals.shares > 0 {
		base := totals.shares*allotPrice + totalCost
		if base > 0 {
			returnBps = realizedFen * 10000 / base
		}
	}

	return View{
		Entry:                e,
		ApplyFundsFen:        applyFunds,
		FreezeDays:           days,
		FreezeEstimated:      freezeEstimated,
		CostFen:              costFen,
		SoldShares:           totals.shares,
		SellAmountFen:        income,
		SellPriceFen:         totals.averagePrice(),
		SellDate:             totals.lastDate,
		SaleCount:            totals.count,
		SellFeeFen:           sellFee,
		FeeEstimated:         totals.feeEstimated,
		CapitalCostFen:       capitalCost,
		CapitalCostEstimated: capitalEstimated,
		TotalCostFen:         totalCost,
		RealizedFen:          realizedFen,
		HoldingShares:        holdingShares,
		HoldingCostFen:       holdingCostFen,
		ReturnBps:            returnBps,
		Status:               e.status(totals.shares),
	}
}

// status 归纳记录所处阶段。
func (e Entry) status(soldShares int64) string {
	switch {
	case e.AllottedShares == 0:
		if e.ApplyShares > 0 {
			return StatusApplied
		}
		return StatusPendingIn
	case soldShares == 0:
		return StatusAllotted
	case soldShares < e.AllottedShares:
		return StatusPartSold
	default:
		return StatusSold
	}
}

// Summary 是账本汇总。
type Summary struct {
	// EntryCount 是记录总数。
	EntryCount int `json:"entryCount"`
	// AppliedIssues 是申购过的新股只数（去重）。
	AppliedIssues int `json:"appliedIssues"`
	// AllottedIssues 是中签过的新股只数（去重）。
	AllottedIssues int `json:"allottedIssues"`
	// AllottedShares 是中签总股数。
	AllottedShares int64 `json:"allottedShares"`
	// ApplyFundsFen 是申购占用资金合计（按笔累加，不是去重后的市值）。
	ApplyFundsFen int64 `json:"applyFundsFen"`
	// CapitalDays 是累计占用资金天数，用「分·天」表示，用于年化计算。
	CapitalDays int64 `json:"capitalDays"`
	// CostFen 是中签部分的缴款总额。
	CostFen int64 `json:"costFen"`
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
	// EstimatedCapitalFen 是其中按利率估算出来的部分。
	EstimatedCapitalFen int64 `json:"estimatedCapitalFen"`
	// TotalCostFen 是全部支出合计。
	TotalCostFen int64 `json:"totalCostFen"`
	// GrossFen 是未扣资金的卖出毛利。
	GrossFen int64 `json:"grossFen"`
	// RealizedFen 是扣除全部支出后的已实现净收益。
	RealizedFen int64 `json:"realizedFen"`
	// HoldingShares 是当前仍持有的中签股数。
	HoldingShares int64 `json:"holdingShares"`
	// HoldingCostFen 是持仓成本合计。
	HoldingCostFen int64 `json:"holdingCostFen"`
	// ReturnBps 是已实现收益率（净收益 ÷ 卖出部分成本与支出之和），单位万分之一。
	ReturnBps int64 `json:"returnBps"`
	// AnnualizedBps 是资金年化回报率：净收益 ÷ 累计占用资金天数 × 365。
	AnnualizedBps int64 `json:"annualizedBps"`
	// WinRateBps 是中签率（中签只数 / 申购只数），单位万分之一。
	WinRateBps int64 `json:"winRateBps"`
	// ByAccount 是按券商账户分组的汇总。
	ByAccount []AccountSummary `json:"byAccount"`
}

// AccountSummary 是单个券商账户的汇总。
type AccountSummary struct {
	BrokerID       int64  `json:"brokerId"`
	Account        string `json:"account"`
	EntryCount     int    `json:"entryCount"`
	AllottedIssues int    `json:"allottedIssues"`
	ApplyFundsFen  int64  `json:"applyFundsFen"`
	SaleCount      int    `json:"saleCount"`
	SellAmountFen  int64  `json:"sellAmountFen"`
	CapitalCostFen int64  `json:"capitalCostFen"`
	RealizedFen    int64  `json:"realizedFen"`
	HoldingShares  int64  `json:"holdingShares"`
	ReturnBps      int64  `json:"returnBps"`
}

// Summarize 汇总记录。相同新股在不同账户各算一只，只数按申购代码去重。
func Summarize(entries []Entry) Summary {
	var s Summary
	s.EntryCount = len(entries)
	applied := make(map[string]bool)
	allotted := make(map[string]bool)
	accounts := make(map[int64]*AccountSummary)
	accountBase := make(map[int64]int64)
	var realizedBase int64

	for _, e := range entries {
		v := e.Compute()
		if e.ApplyShares > 0 || e.AllottedShares > 0 {
			applied[e.FxCode] = true
		}
		if e.AllottedShares > 0 {
			allotted[e.FxCode] = true
		}
		s.AllottedShares += e.AllottedShares
		s.ApplyFundsFen += v.ApplyFundsFen
		s.CapitalDays += v.ApplyFundsFen * v.FreezeDays
		s.CostFen += v.CostFen
		s.SaleCount += v.SaleCount
		s.SellAmountFen += v.SellAmountFen
		s.SellFeeFen += v.SellFeeFen
		s.OtherFeeFen += e.OtherFeeFen
		s.CapitalCostFen += v.CapitalCostFen
		if v.CapitalCostEstimated {
			s.EstimatedCapitalFen += v.CapitalCostFen
		}
		s.TotalCostFen += v.TotalCostFen
		s.RealizedFen += v.RealizedFen
		s.HoldingShares += v.HoldingShares
		s.HoldingCostFen += v.HoldingCostFen
		if v.SoldShares > 0 {
			s.GrossFen += v.SellAmountFen - v.SoldShares*e.allotPrice()
			realizedBase += v.SoldShares*e.allotPrice() + v.TotalCostFen
			accountBase[e.BrokerID] += v.SoldShares*e.allotPrice() + v.TotalCostFen
		}

		acc, ok := accounts[e.BrokerID]
		if !ok {
			acc = &AccountSummary{BrokerID: e.BrokerID, Account: e.Account}
			accounts[e.BrokerID] = acc
		}
		acc.EntryCount++
		acc.ApplyFundsFen += v.ApplyFundsFen
		acc.SaleCount += v.SaleCount
		acc.SellAmountFen += v.SellAmountFen
		acc.CapitalCostFen += v.CapitalCostFen
		acc.RealizedFen += v.RealizedFen
		acc.HoldingShares += v.HoldingShares
		if e.AllottedShares > 0 {
			acc.AllottedIssues++
		}
	}
	for id, base := range accountBase {
		if acc, ok := accounts[id]; ok && base > 0 {
			acc.ReturnBps = acc.RealizedFen * 10000 / base
		}
	}

	s.AppliedIssues = len(applied)
	s.AllottedIssues = len(allotted)
	if s.AppliedIssues > 0 {
		s.WinRateBps = int64(s.AllottedIssues) * 10000 / int64(s.AppliedIssues)
	}
	if realizedBase > 0 {
		s.ReturnBps = s.RealizedFen * 10000 / realizedBase
	}
	// 年化：净收益 ÷ 占用资金天数 × 365，结果用万分之一表示。
	if s.CapitalDays > 0 {
		n := new(big.Int).Mul(big.NewInt(s.RealizedFen), big.NewInt(365*10000))
		n.Quo(n, big.NewInt(s.CapitalDays))
		if n.IsInt64() {
			s.AnnualizedBps = n.Int64()
		}
	}
	for _, acc := range accounts {
		s.ByAccount = append(s.ByAccount, *acc)
	}
	sort.SliceStable(s.ByAccount, func(i, j int) bool {
		if s.ByAccount[i].RealizedFen == s.ByAccount[j].RealizedFen {
			return s.ByAccount[i].Account < s.ByAccount[j].Account
		}
		return s.ByAccount[i].RealizedFen > s.ByAccount[j].RealizedFen
	})
	return s
}

// Fen 把分转换为元的两层结构，便于界面避免浮点格式化。
type Fen int64

// Yuan 返回元的整数部分。
func (f Fen) Yuan() int64 { return int64(f) / 100 }

// Cents 返回分的两位尾数（0-99），负数返回绝对值。
func (f Fen) Cents() int64 {
	v := int64(f) % 100
	if v < 0 {
		v = -v
	}
	return v
}

// Negative 报告金额是否为负。
func (f Fen) Negative() bool { return f < 0 }

// String 输出形如 "-1,234.56" 的金额文本。
func (f Fen) String() string {
	sign := ""
	v := int64(f)
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}
