package ledger

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 一份典型的北交所打新：发行价 4.48，申购 1000 股，中签 500 股，
// 资金冻结两天，券商年化 3.85%、佣金万分之 2.5、最低佣金 5 元。
func sampleEntry() Entry {
	return Entry{
		BrokerID:         1,
		FxCode:           "920306",
		Account:          "测试证券",
		IssuePriceFen:    448,
		PurchaseDate:     "2026-10-20",
		ApplyShares:      1000,
		AllottedShares:   500,
		AllotPriceFen:    448,
		FreezeDate:       "2026-10-20",
		RefundDate:       "2026-10-22",
		AnnualRatePPM:    38500,
		CommissionPPM:    250,
		MinCommissionFen: 500,
		// 500 股一次卖完，成交价 9.20 元。
		Sales: []Sale{{Date: "2026-10-30", Shares: 500, PriceFen: 920}},
	}
}

func TestComputeRealizedWithCapitalCost(t *testing.T) {
	v := sampleEntry().Compute()

	// 申购占用资金 448 × 1000 = 448000 分
	if got, want := v.ApplyFundsFen, int64(448000); got != want {
		t.Errorf("ApplyFundsFen = %d, want %d", got, want)
	}
	if got, want := v.FreezeDays, int64(2); got != want {
		t.Errorf("FreezeDays = %d, want %d", got, want)
	}
	if v.FreezeEstimated {
		t.Error("FreezeEstimated = true, want false（已填退款日）")
	}
	// 资金成本 = 448000 × 38500 × 2 ÷ (1e6 × 365) = 94.5 → 95 分
	if got, want := v.CapitalCostFen, int64(95); got != want {
		t.Errorf("CapitalCostFen = %d, want %d", got, want)
	}
	if !v.CapitalCostEstimated {
		t.Error("CapitalCostEstimated = true, want 按利率估算")
	}
	// 卖出费用按券商最低佣金 500 分计
	if got, want := v.SellFeeFen, int64(500); got != want {
		t.Errorf("SellFeeFen = %d, want %d", got, want)
	}
	if !v.FeeEstimated {
		t.Error("FeeEstimated 应为 true（按佣金率估算）")
	}
	if got, want := v.TotalCostFen, int64(595); got != want {
		t.Errorf("TotalCostFen = %d, want %d", got, want)
	}
	// 净收益 = 460000 − 224000 − 595
	if got, want := v.RealizedFen, int64(235405); got != want {
		t.Errorf("RealizedFen = %d, want %d", got, want)
	}
	if got := v.HoldingShares; got != 0 {
		t.Errorf("HoldingShares = %d, want 0", got)
	}
	if got, want := v.Status, StatusSold; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
	// 收益率 = 235405 ÷ (224000 + 595) = 104.8%
	if got, want := v.ReturnBps, int64(10481); got != want {
		t.Errorf("ReturnBps = %d, want %d", got, want)
	}
}

func TestComputeUsesActualCosts(t *testing.T) {
	e := sampleEntry()
	// 手工填了实际资金成本与卖出费用时，不再按利率和佣金率估算。
	e.CapitalCostFen = 800
	e.Sales[0].FeeFen = 600
	e.OtherFeeFen = 120
	v := e.Compute()
	if v.CapitalCostEstimated || v.FeeEstimated {
		t.Error("填写实际值后不应再标记为估算")
	}
	if got, want := v.CapitalCostFen, int64(800); got != want {
		t.Errorf("CapitalCostFen = %d, want %d", got, want)
	}
	if got, want := v.TotalCostFen, int64(1520); got != want {
		t.Errorf("TotalCostFen = %d, want %d", got, want)
	}
	if got, want := v.RealizedFen, int64(234480); got != want {
		t.Errorf("RealizedFen = %d, want %d", got, want)
	}
}

func TestFreezeDaysFallsBackToDefault(t *testing.T) {
	e := sampleEntry()
	e.RefundDate = ""
	v := e.Compute()
	if !v.FreezeEstimated {
		t.Error("未填退款日时应标记为估算")
	}
	if got, want := v.FreezeDays, int64(defaultFreezeDays); got != want {
		t.Errorf("FreezeDays = %d, want %d", got, want)
	}
	// 默认两天，资金成本应为 0.95 元
	if got, want := v.CapitalCostFen, int64(95); got != want {
		t.Errorf("CapitalCostFen = %d, want %d", got, want)
	}
}

func TestFreezeDaysFromPurchaseDate(t *testing.T) {
	e := sampleEntry()
	e.FreezeDate = ""
	e.RefundDate = "2026-10-23"
	v := e.Compute()
	// 冻结日留空时以申购日（10-20）起算，到 10-23 共 3 天
	if got, want := v.FreezeDays, int64(3); got != want {
		t.Errorf("FreezeDays = %d, want %d", got, want)
	}
}

func TestZeroRateMeansNoCapitalCost(t *testing.T) {
	e := sampleEntry()
	e.AnnualRatePPM = 0
	v := e.Compute()
	if v.CapitalCostFen != 0 || v.CapitalCostEstimated {
		t.Errorf("CapitalCostFen = %d, estimated = %v, want 0/false", v.CapitalCostFen, v.CapitalCostEstimated)
	}
}

func TestCommissionRateAboveMinimum(t *testing.T) {
	// 卖出金额 100 万元，佣金万分之 2.5 得 250 元，高于最低 5 元。
	e := Entry{BrokerID: 1, FxCode: "920001", IssuePriceFen: 1000, AllottedShares: 100000,
		Sales:         []Sale{{Date: "2026-10-30", Shares: 100000, PriceFen: 1000}},
		CommissionPPM: 250, MinCommissionFen: 500}
	v := e.Compute()
	if got, want := v.SellFeeFen, int64(25000); got != want {
		t.Errorf("SellFeeFen = %d, want %d", got, want)
	}
}

func TestComputePartial(t *testing.T) {
	e := Entry{
		BrokerID: 1, FxCode: "920001", Account: "A",
		IssuePriceFen: 1000, AllottedShares: 1000,
		Sales: []Sale{{Date: "2026-10-30", Shares: 400, PriceFen: 1500, FeeFen: 100}},
	}
	v := e.Compute()
	if got, want := v.RealizedFen, int64(199900); got != want {
		t.Errorf("RealizedFen = %d, want %d", got, want)
	}
	if got, want := v.HoldingShares, int64(600); got != want {
		t.Errorf("HoldingShares = %d, want %d", got, want)
	}
	if got, want := v.HoldingCostFen, int64(600000); got != want {
		t.Errorf("HoldingCostFen = %d, want %d", got, want)
	}
	if got, want := v.Status, StatusPartSold; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
}

func TestComputeLoss(t *testing.T) {
	e := Entry{
		BrokerID: 1, FxCode: "920002", Account: "A",
		IssuePriceFen: 2000, AllottedShares: 500,
		Sales: []Sale{{Date: "2026-10-30", Shares: 500, PriceFen: 1500}},
	}
	v := e.Compute()
	if got, want := v.RealizedFen, int64(-250000); got != want {
		t.Errorf("RealizedFen = %d, want %d", got, want)
	}
	if v.ReturnBps >= 0 {
		t.Errorf("ReturnBps = %d, want negative", v.ReturnBps)
	}
}

func TestValidate(t *testing.T) {
	base := Entry{FxCode: "920001", BrokerID: 1}
	cases := []struct {
		name string
		edit func(*Entry)
		want error
	}{
		{"缺少代码", func(e *Entry) { e.FxCode = "" }, ErrMissingCode},
		{"缺少账户", func(e *Entry) { e.BrokerID = 0 }, ErrMissingAccount},
		{"负数", func(e *Entry) { e.AllottedShares = -1 }, ErrNegativeValue},
		{"未中签却卖出", func(e *Entry) {
			e.AllottedShares = 0
			e.Sales = []Sale{{Date: "2026-10-30", Shares: 100, PriceFen: 1000}}
		}, ErrNoAllotment},
		{"卖出超过中签", func(e *Entry) {
			e.AllottedShares = 100
			e.Sales = []Sale{
				{Date: "2026-10-29", Shares: 100, PriceFen: 1000},
				{Date: "2026-10-30", Shares: 100, PriceFen: 1000},
			}
		}, ErrSoldOverAllotted},
		{"退款早于冻结", func(e *Entry) {
			e.FreezeDate = "2026-10-22"
			e.RefundDate = "2026-10-20"
		}, ErrBadDate},
		{"日期格式错", func(e *Entry) { e.FreezeDate = "2026/10/20" }, ErrBadDate},
	}
	for _, tc := range cases {
		e := base
		tc.edit(&e)
		if err := e.Validate(); err != tc.want {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidateDefaults(t *testing.T) {
	e := Entry{FxCode: "920001", BrokerID: 1, IssuePriceFen: 830, AllottedShares: 100,
		PurchaseDate: "2026-10-20"}
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if e.AllotPriceFen != 830 {
		t.Errorf("AllotPriceFen = %d, want 830", e.AllotPriceFen)
	}
	// 冻款日默认取申购日
	if e.FreezeDate != "2026-10-20" {
		t.Errorf("FreezeDate = %q, want 2026-10-20", e.FreezeDate)
	}
}

func TestBrokerValidate(t *testing.T) {
	b := Broker{Name: " 华泰证券 ", Account: ""}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if b.Name != "华泰证券" || b.Account != "默认账户" {
		t.Errorf("Broker = %+v", b)
	}
	if got := b.Label(); got != "华泰证券 · 默认账户" {
		t.Errorf("Label = %q", got)
	}
	if err := (&Broker{}).Validate(); err != ErrMissingBroker {
		t.Errorf("err = %v, want ErrMissingBroker", err)
	}
	if err := (&Broker{Name: "A", AnnualRatePPM: 100 * rateScale}).Validate(); err != ErrRateRange {
		t.Errorf("err = %v, want ErrRateRange", err)
	}
}

func TestSummarize(t *testing.T) {
	entries := []Entry{
		{BrokerID: 1, Account: "A", FxCode: "920001", IssuePriceFen: 1000, PurchaseDate: "2026-10-20",
			FreezeDate: "2026-10-20", RefundDate: "2026-10-22", AnnualRatePPM: 36500,
			ApplyShares: 500, AllottedShares: 500,
			Sales: []Sale{{Date: "2026-10-30", Shares: 500, PriceFen: 2000, FeeFen: 100}}},
		{BrokerID: 1, Account: "A", FxCode: "920002", IssuePriceFen: 1000, ApplyShares: 500},
		{BrokerID: 2, Account: "B", FxCode: "920003", IssuePriceFen: 1000, AllottedShares: 1000},
	}
	s := Summarize(entries)
	if s.EntryCount != 3 {
		t.Errorf("EntryCount = %d, want 3", s.EntryCount)
	}
	if s.AppliedIssues != 3 {
		t.Errorf("AppliedIssues = %d, want 3", s.AppliedIssues)
	}
	if s.AllottedIssues != 2 {
		t.Errorf("AllottedIssues = %d, want 2", s.AllottedIssues)
	}
	if s.AllottedShares != 1500 {
		t.Errorf("AllottedShares = %d, want 1500", s.AllottedShares)
	}
	// 第一笔：卖出 500×2000=1000000，成本 500000，资金成本 1000×500×36500×2/(1e6×365)=100，费用 100
	if got, want := s.RealizedFen, int64(499800); got != want {
		t.Errorf("RealizedFen = %d, want %d", got, want)
	}
	if got, want := s.CapitalCostFen, int64(100); got != want {
		t.Errorf("CapitalCostFen = %d, want %d", got, want)
	}
	if s.HoldingShares != 1000 {
		t.Errorf("HoldingShares = %d, want 1000", s.HoldingShares)
	}
	// 占用资金天数：第一笔 500×1000×2 天，第二笔按默认 2 天
	wantDays := int64(500*1000*2 + 500*1000*2)
	if s.CapitalDays != wantDays {
		t.Errorf("CapitalDays = %d, want %d", s.CapitalDays, wantDays)
	}
	if s.AnnualizedBps == 0 {
		t.Error("AnnualizedBps 应为非 0")
	}
	if len(s.ByAccount) != 2 {
		t.Fatalf("ByAccount = %d, want 2", len(s.ByAccount))
	}
	if s.ByAccount[0].Account != "A" {
		t.Errorf("first account = %q, want A", s.ByAccount[0].Account)
	}
}

// 一笔新股分三次卖：股数与金额逐笔相加，费用各笔单独算。
//
// 保底佣金是按笔收的，分三笔就会多掏两次，本测试把这个差别钉住。
// TestComputeEmptySales 检查没卖出时 Sales 是空数组而不是 nil。
//
// 接口把 View 直接序列化给 agent，`null` 会让读的人多写一次判空。
func TestComputeEmptySales(t *testing.T) {
	e := Entry{BrokerID: 1, FxCode: "920009", IssuePriceFen: 1000, AllottedShares: 100}
	v := e.Compute()
	if v.Sales == nil {
		t.Fatal("没卖出时 Sales 应为空数组")
	}
	if len(v.Sales) != 0 || v.SaleCount != 0 || v.SoldShares != 0 || v.Status != "已中签" {
		t.Errorf("视图 = %+v", v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"sales":[]`) {
		t.Errorf("JSON 里 sales 应为 []，实际 %s", raw)
	}
}

func TestComputeMultipleSales(t *testing.T) {
	e := Entry{
		BrokerID: 1, FxCode: "920009",
		IssuePriceFen: 1000, AllottedShares: 1500,
		CommissionPPM: 250, MinCommissionFen: 500,
		Sales: []Sale{
			{Date: "2026-10-28", Shares: 500, PriceFen: 1200},
			{Date: "2026-10-29", Shares: 500, PriceFen: 1500},
			// 填了金额就以金额为准，成交价只用于展示。
			{Date: "2026-10-30", Shares: 500, PriceFen: 180, AmountFen: 90000},
		},
	}
	v := e.Compute()
	if v.SaleCount != 3 || v.SoldShares != 1500 {
		t.Fatalf("笔数/股数 = %d/%d, want 3/1500", v.SaleCount, v.SoldShares)
	}
	// 600000 + 750000 + 90000
	if got, want := v.SellAmountFen, int64(1440000); got != want {
		t.Errorf("SellAmountFen = %d, want %d", got, want)
	}
	// 三笔都只到 5 元保底：500 × 3
	if got, want := v.SellFeeFen, int64(1500); got != want {
		t.Errorf("SellFeeFen = %d, want %d", got, want)
	}
	if !v.FeeEstimated {
		t.Error("FeeEstimated 应为 true（全部按佣金率估算）")
	}
	// 1440000 − 1500000 − 1500
	if got, want := v.RealizedFen, int64(-61500); got != want {
		t.Errorf("RealizedFen = %d, want %d", got, want)
	}
	if got, want := v.SellDate, "2026-10-30"; got != want {
		t.Errorf("SellDate = %q, want %q", got, want)
	}
	// 均价用于展示：1440000 ÷ 1500 = 960
	if got, want := v.SellPriceFen, int64(960); got != want {
		t.Errorf("SellPriceFen = %d, want %d", got, want)
	}
	if got, want := v.Status, StatusSold; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
}

// 卖出明细本身也要挡错：股数、价格、日期、笔数。
func TestValidateSaleRules(t *testing.T) {
	base := Entry{FxCode: "920001", BrokerID: 1, AllottedShares: 1000}
	cases := []struct {
		name  string
		sales []Sale
		want  error
	}{
		{"股数为 0", []Sale{{Shares: 0, PriceFen: 1000}}, ErrSaleShares},
		{"没有价格也没有金额", []Sale{{Shares: 100}}, ErrSalePrice},
		{"金额为负", []Sale{{Shares: 100, AmountFen: -1}}, ErrNegativeValue},
		{"日期格式不对", []Sale{{Shares: 100, PriceFen: 1000, Date: "2026/10/30"}}, ErrBadDate},
	}
	for _, tc := range cases {
		e := base
		e.Sales = tc.sales
		if err := e.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// 卖出日留空时补成今天，不会被当成错误。
	e := base
	e.Sales = []Sale{{Shares: 100, PriceFen: 1000}}
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if e.Sales[0].Date == "" {
		t.Error("空卖出日应补成今天")
	}
	// 超过笔数上限直接拒绝，挡住脚本重复追加。
	e = base
	for i := 0; i < MaxSales+1; i++ {
		e.Sales = append(e.Sales, Sale{Shares: 1, PriceFen: 1000})
	}
	if err := e.Validate(); !errors.Is(err, ErrTooManySales) {
		t.Errorf("err = %v, want ErrTooManySales", err)
	}
}

func TestInterestRounding(t *testing.T) {
	// 10000 元（1000000 分）按 3.65% 计一年，正好 365 元。
	if got, want := interest(1000000, 36500, 365), int64(36500); got != want {
		t.Errorf("interest = %d, want %d", got, want)
	}
	// 不足一分时舍去
	if got := interest(100, 36500, 1); got != 0 {
		t.Errorf("interest = %d, want 0", got)
	}
	if got := interest(0, 36500, 30); got != 0 {
		t.Errorf("interest(0) = %d, want 0", got)
	}
}

func TestFenString(t *testing.T) {
	cases := []struct {
		in   Fen
		want string
	}{
		{448, "4.48"},
		{0, "0.00"},
		{-123456, "-1234.56"},
		{5, "0.05"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Fen(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}
