package ledger

import (
	"testing"
)

// statsEntries 造三笔不同年份、不同账户的记录，外加一笔没填申购日的。
//
// 每笔都是「当日申购、两日后回款、一周后卖出」的常见节奏，金额刻意取整，
// 便于手算核对。
func statsEntries() []Entry {
	return []Entry{
		// 2026-10-20，账户 1：中签 500 股，6.20 元卖出。
		{BrokerID: 1, Account: "华泰证券", FxCode: "920001", Name: "十月一",
			IssuePriceFen: 500, ApplyShares: 1000, AllottedShares: 500,
			PurchaseDate: "2026-10-20", FreezeDate: "2026-10-20", RefundDate: "2026-10-22",
			AnnualRatePPM: 36500, CommissionPPM: 250, MinCommissionFen: 500,
			Sales: []Sale{{Date: "2026-10-28", Shares: 500, PriceFen: 620}}},
		// 2026-10-21，账户 2：中签 1000 股，10.00 元卖出。
		{BrokerID: 2, Account: "中信证券", FxCode: "920002", Name: "十月二",
			IssuePriceFen: 1000, ApplyShares: 2000, AllottedShares: 1000,
			PurchaseDate: "2026-10-21", FreezeDate: "2026-10-21", RefundDate: "2026-10-23",
			AnnualRatePPM: 36500, CommissionPPM: 250, MinCommissionFen: 500,
			Sales: []Sale{{Date: "2026-10-29", Shares: 1000, PriceFen: 1000}}},
		// 2025-12-15，账户 1：中签 200 股，还没卖。
		{BrokerID: 1, Account: "华泰证券", FxCode: "920003", Name: "去年一只",
			IssuePriceFen: 800, ApplyShares: 500, AllottedShares: 200,
			PurchaseDate: "2025-12-15", FreezeDate: "2025-12-15", RefundDate: "2025-12-17",
			AnnualRatePPM: 36500},
		// 没填申购日：只能进「全部」，进不了按年按月。
		{BrokerID: 2, Account: "中信证券", FxCode: "920004", Name: "没写日期",
			IssuePriceFen: 300, ApplyShares: 100, PurchaseDate: ""},
	}
}

func TestBuildStatsByYear(t *testing.T) {
	all := BuildStats(statsEntries(), StatsFilter{})

	// 年份下拉框列的是账本全集，从新到旧。
	if len(all.Years) != 2 || all.Years[0] != 2026 || all.Years[1] != 2025 {
		t.Fatalf("Years = %v, want [2026 2025]", all.Years)
	}
	if all.Summary.EntryCount != 4 {
		t.Errorf("EntryCount = %d, want 4", all.Summary.EntryCount)
	}
	// 2026 / 2025 / 未填申购日，共三组，没日期的垫底。
	if len(all.ByYear) != 3 {
		t.Fatalf("ByYear 组数 = %d, want 3", len(all.ByYear))
	}
	if all.ByYear[0].Key != "2026" || all.ByYear[0].EntryCount != 2 {
		t.Errorf("第一组 = %+v, want 2026 年 2 条", all.ByYear[0])
	}
	if all.ByYear[1].Key != "2025" || all.ByYear[1].EntryCount != 1 {
		t.Errorf("第二组 = %+v, want 2025 年 1 条", all.ByYear[1])
	}
	if !all.ByYear[2].Undated || all.ByYear[2].EntryCount != 1 {
		t.Errorf("最后一组应为「未填申购日」= %+v", all.ByYear[2])
	}

	// 按年分组合计必须等于总计，一分不差。
	var realized, funds, sell int64
	for _, b := range all.ByYear {
		realized += b.RealizedFen
		funds += b.ApplyFundsFen
		sell += b.SellAmountFen
	}
	if realized != all.Summary.RealizedFen {
		t.Errorf("按年净收益合计 %d ≠ 总计 %d", realized, all.Summary.RealizedFen)
	}
	if funds != all.Summary.ApplyFundsFen {
		t.Errorf("按年申购资金合计 %d ≠ 总计 %d", funds, all.Summary.ApplyFundsFen)
	}
	if sell != all.Summary.SellAmountFen {
		t.Errorf("按年卖出金额合计 %d ≠ 总计 %d", sell, all.Summary.SellAmountFen)
	}
}

func TestBuildStatsFilterByBroker(t *testing.T) {
	stats := BuildStats(statsEntries(), StatsFilter{BrokerID: 2})
	if stats.Summary.EntryCount != 2 {
		t.Fatalf("EntryCount = %d, want 2", stats.Summary.EntryCount)
	}
	if len(stats.Accounts) != 2 {
		t.Errorf("账户下拉框应列出全部账户，得到 %d 个", len(stats.Accounts))
	}
	for _, acc := range stats.Summary.ByAccount {
		if acc.BrokerID != 2 {
			t.Errorf("筛选后仍出现账户 %d", acc.BrokerID)
		}
	}
	// 账户 2 只有一只中签、一只未中签，卖出金额只算卖出那只。
	if got, want := stats.Summary.SaleCount, 1; got != want {
		t.Errorf("SaleCount = %d, want %d", got, want)
	}
	if stats.Summary.SellAmountFen != 1000000 {
		t.Errorf("SellAmountFen = %d, want 1000000", stats.Summary.SellAmountFen)
	}
}

func TestBuildStatsFilterByMonth(t *testing.T) {
	all := BuildStats(statsEntries(), StatsFilter{Year: 2026, Month: 10})
	if all.Summary.EntryCount != 2 {
		t.Fatalf("2026-10 EntryCount = %d, want 2", all.Summary.EntryCount)
	}
	if len(all.ByYear) != 1 || all.ByYear[0].Key != "2026" {
		t.Errorf("ByYear = %+v, want 只有 2026", all.ByYear)
	}
	// 月度分布不吃年份与月份筛选（见 TestBuildStatsMonthBucketsIgnoreYearFilter），
	// 这里只确认筛出来的那个月仍排在最前面。
	if all.ByMonth[0].Key != "2026-10" {
		t.Errorf("ByMonth[0].Key = %q, want 2026-10", all.ByMonth[0].Key)
	}

	// 只给月份不给年份：跨年取所有该月份。
	onlyMonth := BuildStats(statsEntries(), StatsFilter{Month: 10})
	if onlyMonth.Summary.EntryCount != 2 {
		t.Errorf("只筛 10 月 EntryCount = %d, want 2", onlyMonth.Summary.EntryCount)
	}
	// 月度分布这时看的是「所有年份的 10 月」，不只 2026。没填申购日的记录
	// 也单列一组——它没法归月，但提醒账本里还有缺日期的记录。
	if len(onlyMonth.ByMonth) != 3 {
		t.Fatalf("月度分布组数 = %d, want 3（含未填申购日）", len(onlyMonth.ByMonth))
	}
	if onlyMonth.ByMonth[0].Key != "2026-10" || onlyMonth.ByMonth[1].Key != "2025-12" {
		t.Errorf("月度分布顺序 = %q, %q, want 2026-10, 2025-12",
			onlyMonth.ByMonth[0].Key, onlyMonth.ByMonth[1].Key)
	}
	if !onlyMonth.ByMonth[2].Undated {
		t.Errorf("最后一组应为「未填申购日」，得到 %+v", onlyMonth.ByMonth[2])
	}
}

func TestBuildStatsMonthBucketsIgnoreYearFilter(t *testing.T) {
	// 在月度分布里点了 2026-10 之后，年份跟着变成 2026；别的年份的月份
	// 不能因此消失，否则想再看 2025-12 只能先重置筛选。
	stats := BuildStats(statsEntries(), StatsFilter{Year: 2026, Month: 10})
	if got := len(stats.ByMonth); got != 3 {
		t.Fatalf("ByMonth 组数 = %d, want 3（含 2025-12 与未填申购日）", got)
	}
	if stats.ByMonth[0].Key != "2026-10" || stats.ByMonth[1].Key != "2025-12" {
		t.Errorf("ByMonth 顺序 = %q, %q, want 2026-10, 2025-12",
			stats.ByMonth[0].Key, stats.ByMonth[1].Key)
	}
	// 2025-12 那一组的数字必须是那一条记录的，不被年份筛选改口径。
	if b := stats.ByMonth[1]; b.EntryCount != 1 || b.HoldingShares != 200 {
		t.Errorf("2025-12 分组 = %+v, want 1 条、持仓 200 股", b)
	}

	// 账户筛选仍然生效：月度分布只放开时间条件。
	byBroker := BuildStats(statsEntries(), StatsFilter{BrokerID: 1, Year: 2026})
	for _, b := range byBroker.ByMonth {
		if b.Undated {
			continue
		}
		// 账户 1 只有 2026-10 与 2025-12 两个月份。
		if b.Key != "2026-10" && b.Key != "2025-12" {
			t.Errorf("账户 1 的月度分布里出现了 %q", b.Key)
		}
	}
	if len(byBroker.ByMonth) != 2 {
		t.Errorf("账户 1 的 ByMonth 组数 = %d, want 2", len(byBroker.ByMonth))
	}
}

func TestBuildStatsMonthBucketsIgnoreMonthFilter(t *testing.T) {
	// 选 2025 年 12 月：卡片只剩那一条，月度分布里那个月的数字要与它逐分相等，
	// 不能因为「正在筛这个月」就算重或算漏。
	stats := BuildStats(statsEntries(), StatsFilter{Year: 2025, Month: 12})
	if stats.Summary.EntryCount != 1 {
		t.Fatalf("EntryCount = %d, want 1", stats.Summary.EntryCount)
	}
	var picked *Bucket
	for i := range stats.ByMonth {
		if stats.ByMonth[i].Key == "2025-12" {
			picked = &stats.ByMonth[i]
		}
	}
	if picked == nil {
		t.Fatalf("月度分布里没有 2025-12：%+v", stats.ByMonth)
	}
	if picked.EntryCount != 1 || picked.ApplyFundsFen != stats.Summary.ApplyFundsFen {
		t.Errorf("2025-12 分组 = %+v, want 与总计同口径", *picked)
	}
	// 没卖出的记录不该有净收益，也不该被算成亏损。
	if stats.Summary.RealizedFen != 0 {
		t.Errorf("RealizedFen = %d, want 0", stats.Summary.RealizedFen)
	}
	if stats.Summary.HoldingShares != 200 {
		t.Errorf("HoldingShares = %d, want 200", stats.Summary.HoldingShares)
	}
}

func TestBuildStatsYearBucketReturnRate(t *testing.T) {
	stats := BuildStats(statsEntries(), StatsFilter{Year: 2026})
	var bucket Bucket
	for _, b := range stats.ByYear {
		if b.Key == "2026" {
			bucket = b
		}
	}
	if bucket.EntryCount != 2 {
		t.Fatalf("2026 年 EntryCount = %d, want 2", bucket.EntryCount)
	}
	// 收益率与总计同一口径：净收益 ÷（卖出缴款成本 + 支出）。
	if bucket.ReturnBps != stats.Summary.ReturnBps {
		t.Errorf("分年收益率 %d ≠ 总计 %d", bucket.ReturnBps, stats.Summary.ReturnBps)
	}
	if bucket.SaleCount != 2 {
		t.Errorf("SaleCount = %d, want 2", bucket.SaleCount)
	}
	if bucket.AllottedIssues != 2 {
		t.Errorf("AllottedIssues = %d, want 2", bucket.AllottedIssues)
	}
}

func TestBuildStatsDetails(t *testing.T) {
	// 不给筛选条件：明细是「全部」的记录，逐条相加要等于总计。
	all := BuildStats(statsEntries(), StatsFilter{})
	if len(all.Details) != 4 {
		t.Fatalf("Details 条数 = %d, want 4", len(all.Details))
	}

	var realized, funds, sell int64
	for _, row := range all.Details {
		realized += row.RealizedFen
		funds += row.ApplyFundsFen
		sell += row.SellAmountFen
	}
	if realized != all.Summary.RealizedFen {
		t.Errorf("明细净收益合计 %d ≠ 总计 %d", realized, all.Summary.RealizedFen)
	}
	if funds != all.Summary.ApplyFundsFen {
		t.Errorf("明细申购资金合计 %d ≠ 总计 %d", funds, all.Summary.ApplyFundsFen)
	}
	if sell != all.Summary.SellAmountFen {
		t.Errorf("明细卖出金额合计 %d ≠ 总计 %d", sell, all.Summary.SellAmountFen)
	}

	// 顺序跟「我的打新」列表一致：申购日从新到旧，没填日期的垫底。
	wantOrder := []string{"920002", "920001", "920003", "920004"}
	for i, want := range wantOrder {
		if all.Details[i].FxCode != want {
			t.Fatalf("第 %d 行 = %q, want %q（整序：%v）", i, all.Details[i].FxCode, want, detailCodes(all.Details))
		}
	}
	if last := all.Details[3]; last.PurchaseDate != "" || last.ApplyFundsFen != 30000 {
		t.Errorf("没填申购日的那条 = %+v, want 垫底且金额照算", last)
	}
}

func TestBuildStatsDetailsFollowFilter(t *testing.T) {
	// 点某个月看明细：明细必须正好是这个月的那几条，且与筛选后的合计对得上。
	one := BuildStats(statsEntries(), StatsFilter{Year: 2026, Month: 10})
	if len(one.Details) != 2 {
		t.Fatalf("2026-10 明细条数 = %d, want 2", len(one.Details))
	}
	var realized int64
	for _, row := range one.Details {
		if row.PurchaseDate[:7] != "2026-10" {
			t.Errorf("明细混进了别的月份：%+v", row)
		}
		if row.Account == "" {
			t.Errorf("明细缺账户：%+v", row)
		}
		realized += row.RealizedFen
	}
	if realized != one.Summary.RealizedFen {
		t.Errorf("明细净收益合计 %d ≠ 这个月的合计 %d", realized, one.Summary.RealizedFen)
	}
	// 明细与「按月」那一组的数字是同一笔账，只换了看的角度。
	for _, b := range one.ByMonth {
		if b.Key != "2026-10" {
			continue
		}
		if b.RealizedFen != realized {
			t.Errorf("月份组净收益 %d ≠ 明细合计 %d", b.RealizedFen, realized)
		}
	}

	// 只筛账户时不按时间筛，没日期的记录也在明细里。
	byBroker := BuildStats(statsEntries(), StatsFilter{BrokerID: 2})
	if len(byBroker.Details) != 2 {
		t.Fatalf("账户 2 明细条数 = %d, want 2", len(byBroker.Details))
	}
	for _, row := range byBroker.Details {
		if row.BrokerID != 2 {
			t.Errorf("账户筛选后仍出现账户 %d", row.BrokerID)
		}
	}
}

func detailCodes(rows []StatsEntry) []string {
	codes := make([]string, 0, len(rows))
	for _, row := range rows {
		codes = append(codes, row.FxCode)
	}
	return codes
}

func TestStatsFilterValid(t *testing.T) {
	cases := []struct {
		name   string
		filter StatsFilter
		ok     bool
	}{
		{"默认", StatsFilter{}, true},
		{"只有年份", StatsFilter{Year: 2026}, true},
		{"只有月份", StatsFilter{Month: 10}, true},
		{"年份越界", StatsFilter{Year: 1800}, false},
		{"月份越界", StatsFilter{Month: 13}, false},
		{"账户为负", StatsFilter{BrokerID: -1}, false},
	}
	for _, c := range cases {
		if err := c.filter.Valid(); (err == nil) != c.ok {
			t.Errorf("%s: Valid() = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
