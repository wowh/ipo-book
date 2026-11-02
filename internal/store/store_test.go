package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/wowh/ipo-book/internal/ledger"
	"github.com/wowh/ipo-book/internal/source"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newBroker 建一个券商账户，返回其 ID。
func newBroker(t *testing.T, s *Store, name, account string) ledger.Broker {
	t.Helper()
	b, err := s.SaveBroker(context.Background(), ledger.Broker{
		Name: name, Account: account,
		CommissionPPM: 250, MinCommissionFen: 500, AnnualRatePPM: 38500,
	})
	if err != nil {
		t.Fatalf("SaveBroker: %v", err)
	}
	return b
}

func TestSaveAndListIssues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	issues := []source.Issue{
		{FxCode: "920306", StockCode: "874158", Name: "天元重工", PriceFen: 448,
			PurchaseDate: "2026-10-20", ResultDate: "2026-10-23"},
		{FxCode: "920285", StockCode: "874235", Name: "远大健科", PriceFen: 2030,
			PurchaseDate: "2026-10-14", ListingDate: "2026-10-23"},
	}
	if err := s.SaveIssues(ctx, issues, "bse"); err != nil {
		t.Fatalf("SaveIssues: %v", err)
	}
	got, err := s.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].FxCode != "920306" {
		t.Errorf("first = %s, want 920306", got[0].FxCode)
	}
	if s.IssuesUpdatedAt(ctx) == "" {
		t.Error("IssuesUpdatedAt should be set")
	}
}

func TestSaveIssuesKeepsPricedData(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first := []source.Issue{{FxCode: "920306", Name: "天元重工", PriceFen: 448}}
	if err := s.SaveIssues(ctx, first, "bse"); err != nil {
		t.Fatalf("SaveIssues: %v", err)
	}
	second := []source.Issue{{FxCode: "920306", Name: "天元重工"}}
	if err := s.SaveIssues(ctx, second, "bseinfo"); err != nil {
		t.Fatalf("SaveIssues: %v", err)
	}
	got, err := s.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if got[0].PriceFen != 448 {
		t.Errorf("PriceFen = %d, want 448", got[0].PriceFen)
	}
}

func TestBrokerLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	b := newBroker(t, s, "华泰证券", "主账户")
	if b.ID == 0 {
		t.Fatal("ID should be assigned")
	}
	if b.CreatedAt == "" || b.UpdatedAt == "" {
		t.Error("timestamps should be set")
	}

	// 同一个券商 + 账户不能重复
	if _, err := s.SaveBroker(ctx, ledger.Broker{Name: "华泰证券", Account: "主账户"}); err == nil {
		t.Fatal("duplicate broker should be rejected")
	}
	// 同名券商的不同账户可以共存
	if _, err := s.SaveBroker(ctx, ledger.Broker{Name: "华泰证券", Account: "备用"}); err != nil {
		t.Fatalf("second account: %v", err)
	}

	b.AnnualRatePPM = 40000
	updated, err := s.SaveBroker(ctx, b)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.AnnualRatePPM != 40000 {
		t.Errorf("AnnualRatePPM = %d, want 40000", updated.AnnualRatePPM)
	}

	list, err := s.ListBrokers(ctx)
	if err != nil {
		t.Fatalf("ListBrokers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
}

func TestDeleteBrokerInUse(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	b := newBroker(t, s, "中信证券", "主账户")
	if _, err := s.SaveEntry(ctx, ledger.Entry{
		BrokerID: b.ID, FxCode: "920306", Name: "天元重工", IssuePriceFen: 448,
	}); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	if err := s.DeleteBroker(ctx, b.ID); err == nil {
		t.Fatal("broker in use should not be deletable")
	}
}

func TestSaveEntryLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ht := newBroker(t, s, "华泰证券", "主账户")
	citic := newBroker(t, s, "中信证券", "主账户")

	entry := ledger.Entry{
		BrokerID: ht.ID, FxCode: "920306", Name: "天元重工",
		IssuePriceFen: 448, PurchaseDate: "2026-10-20", ApplyShares: 1000,
	}
	saved, err := s.SaveEntry(ctx, entry)
	if err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	if saved.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	// 同账户重复录入被拒绝
	if _, err := s.SaveEntry(ctx, entry); err != ledger.ErrDuplicateEntry {
		t.Fatalf("duplicate err = %v, want ErrDuplicateEntry", err)
	}

	// 不同账户可以各自记录
	other := entry
	other.BrokerID = citic.ID
	if _, err := s.SaveEntry(ctx, other); err != nil {
		t.Fatalf("other account: %v", err)
	}

	// 补填中签、资金成本与两笔卖出
	saved.AllottedShares = 500
	saved.CapitalCostFen = 95
	saved.Sales = []ledger.Sale{
		{Date: "2026-10-29", Shares: 200, PriceFen: 900},
		{Date: "2026-10-30", Shares: 300, PriceFen: 920, FeeFen: 500},
	}
	updated, err := s.SaveEntry(ctx, saved)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.AllottedShares != 500 || updated.CapitalCostFen != 95 {
		t.Errorf("updated = %+v", updated)
	}
	if updated.CreatedAt == "" || updated.UpdatedAt == "" {
		t.Error("timestamps should be set")
	}

	list, err := s.ListEntries(ctx)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	// 明细按日期排在记录上，每笔都有自己的 ID，删单笔才有依据。
	kept := list[0]
	if kept.ID != saved.ID {
		kept = list[1]
	}
	if len(kept.Sales) != 2 {
		t.Fatalf("卖出笔数 = %d, want 2", len(kept.Sales))
	}
	if kept.Sales[0].Shares != 200 || kept.Sales[0].PriceFen != 900 || kept.Sales[0].FeeFen != 0 {
		t.Errorf("第一笔 = %+v", kept.Sales[0])
	}
	if kept.Sales[1].FeeFen != 500 || kept.Sales[1].Date != "2026-10-30" {
		t.Errorf("第二笔 = %+v", kept.Sales[1])
	}
	if kept.Sales[0].ID == 0 {
		t.Error("明细 ID 应由库分配")
	}
	if got, want := kept.Compute().SellAmountFen, int64(200*900+300*920); got != want {
		t.Errorf("成交金额 = %d, want %d", got, want)
	}

	if err := s.DeleteEntry(ctx, saved.ID); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
	if err := s.DeleteEntry(ctx, saved.ID); err == nil {
		t.Fatal("deleting twice should fail")
	}
}

// TestSaleIDsStableAcrossSave 检查重复保存不会让已有明细换 ID。
//
// 接口是按 saleId 定位某一笔去改或删的，如果整条保存一次 ID 就整体漂移，
// agent 手里的 ID 立刻就失效了。
func TestSaleIDsStableAcrossSave(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	broker, err := s.SaveBroker(ctx, ledger.Broker{Name: "华泰", CommissionPPM: 250, MinCommissionFen: 500})
	if err != nil {
		t.Fatalf("SaveBroker: %v", err)
	}
	entry := ledger.Entry{
		BrokerID: broker.ID, FxCode: "920306", Name: "天元重工",
		IssuePriceFen: 448, AllottedShares: 1500, PurchaseDate: "2026-10-20",
		Sales: []ledger.Sale{
			{Date: "2026-10-28", Shares: 400, PriceFen: 620},
			{Date: "2026-10-30", Shares: 600, PriceFen: 620},
		},
	}
	saved, err := s.SaveEntry(ctx, entry)
	if err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	first, second := saved.Sales[0].ID, saved.Sales[1].ID
	if first == 0 || second == 0 || first == second {
		t.Fatalf("明细 ID = %d/%d，应为两个非零且不相同的值", first, second)
	}

	// 改第一笔的成交价、再追加一笔：前两笔的 ID 必须原样保留。
	saved.Sales[0].PriceFen = 630
	saved.Sales = append(saved.Sales, ledger.Sale{Date: "2026-11-02", Shares: 100, PriceFen: 610})
	again, err := s.SaveEntry(ctx, saved)
	if err != nil {
		t.Fatalf("再次保存: %v", err)
	}
	if len(again.Sales) != 3 {
		t.Fatalf("明细 = %d 笔, want 3", len(again.Sales))
	}
	if again.Sales[0].ID != first || again.Sales[1].ID != second {
		t.Errorf("已有明细换 ID 了：%d/%d，want %d/%d",
			again.Sales[0].ID, again.Sales[1].ID, first, second)
	}
	if again.Sales[2].ID == 0 {
		t.Error("新追加的明细应由库分配 ID")
	}

	// 删掉中间一笔，剩下一笔的 ID 也不该变。
	dropped := []ledger.Sale{again.Sales[0], again.Sales[2]}
	last, err := s.SaveEntry(ctx, ledger.Entry{
		ID: again.ID, BrokerID: again.BrokerID, FxCode: again.FxCode, Name: again.Name,
		IssuePriceFen: again.IssuePriceFen, AllottedShares: again.AllottedShares,
		PurchaseDate: again.PurchaseDate, Sales: dropped,
	})
	if err != nil {
		t.Fatalf("删除单笔后保存: %v", err)
	}
	if len(last.Sales) != 2 {
		t.Fatalf("删除后 = %d 笔, want 2", len(last.Sales))
	}
	if last.Sales[0].ID != first || last.Sales[1].ID != again.Sales[2].ID {
		t.Errorf("删除后 ID = %d/%d，want %d/%d",
			last.Sales[0].ID, last.Sales[1].ID, first, again.Sales[2].ID)
	}
}

func TestSaveEntryValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.SaveEntry(ctx, ledger.Entry{FxCode: "", BrokerID: 1}); err != ledger.ErrMissingCode {
		t.Fatalf("err = %v, want ErrMissingCode", err)
	}
	if _, err := s.SaveEntry(ctx, ledger.Entry{FxCode: "920001"}); err != ledger.ErrMissingAccount {
		t.Fatalf("err = %v, want ErrMissingAccount", err)
	}
}

// TestMigrateLegacyAccounts 检查旧版本账本能平滑升级：
// 原来的账户名会被建成券商账户，记录自动关联过去，账户名保持可读。
func TestMigrateLegacyAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	stmts := []string{
		`CREATE TABLE entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			fx_code TEXT NOT NULL,
			stock_code TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			account TEXT NOT NULL DEFAULT '',
			issue_price_fen INTEGER NOT NULL DEFAULT 0,
			apply_shares INTEGER NOT NULL DEFAULT 0,
			allotted_shares INTEGER NOT NULL DEFAULT 0,
			allot_price_fen INTEGER NOT NULL DEFAULT 0,
			fee_fen INTEGER NOT NULL DEFAULT 0,
			sell_price_fen INTEGER NOT NULL DEFAULT 0,
			sold_shares INTEGER NOT NULL DEFAULT 0,
			sell_date TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '')`,
		`CREATE UNIQUE INDEX entries_account_issue ON entries (fx_code, account)`,
		`INSERT INTO entries (fx_code, name, account, issue_price_fen, fee_fen,
				sell_price_fen, sold_shares, sell_date)
			VALUES ('920306', '天元重工', '华泰', 448, 500, 920, 500, '2026-10-30')`,
		`INSERT INTO entries (fx_code, name, account, issue_price_fen)
			VALUES ('920306', '天元重工', '中信', 448)`,
		`INSERT INTO entries (fx_code, name, account, issue_price_fen)
			VALUES ('920285', '远大健科', '', 2030)`,
	}
	for _, stmt := range stmts {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	brokers, err := s.ListBrokers(ctx)
	if err != nil {
		t.Fatalf("ListBrokers: %v", err)
	}
	if len(brokers) != 3 {
		t.Fatalf("brokers = %d, want 3（华泰、中信、默认账户）", len(brokers))
	}
	entries, err := s.ListEntries(ctx)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	for _, e := range entries {
		if e.BrokerID == 0 {
			t.Errorf("entry %s 没有关联券商账户", e.FxCode)
		}
		if e.Account == "" {
			t.Errorf("entry %s 账户名为空", e.FxCode)
		}
		// 老的「一笔卖出」升级成一条成交明细：股数、价格、费用、日期都带过来。
		if e.FxCode == "920306" && e.Account == "华泰" {
			if len(e.Sales) != 1 {
				t.Fatalf("卖出明细 = %d 笔, want 1", len(e.Sales))
			}
			sale := e.Sales[0]
			if sale.Shares != 500 || sale.PriceFen != 920 || sale.FeeFen != 500 || sale.Date != "2026-10-30" {
				t.Errorf("迁移出的明细 = %+v", sale)
			}
			if got, want := e.Compute().SellAmountFen, int64(500*920); got != want {
				t.Errorf("成交金额 = %d, want %d", got, want)
			}
		}
	}

	// 旧列已清零，重启后不会把同一笔再迁移一次。
	var legacyShares int64
	if err := s.db.QueryRow(`SELECT sold_shares FROM entries WHERE fx_code = '920306' AND account = '华泰'`).
		Scan(&legacyShares); err != nil {
		t.Fatalf("查询旧列: %v", err)
	}
	if legacyShares != 0 {
		t.Errorf("旧列 sold_shares = %d, want 0", legacyShares)
	}

	// 升级后唯一约束按券商账户判断：同账户不能重复
	if _, err := s.SaveEntry(ctx, ledger.Entry{
		BrokerID: entries[0].BrokerID, FxCode: entries[0].FxCode,
	}); err != ledger.ErrDuplicateEntry {
		t.Errorf("err = %v, want ErrDuplicateEntry", err)
	}

	// 关掉再打开：迁移是幂等的，不会把同一笔卖出再插一遍。
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	rows, err := again.ListEntries(context.Background())
	if err != nil {
		t.Fatalf("ListEntries after reopen: %v", err)
	}
	for _, e := range rows {
		if e.FxCode == "920306" && e.Account == "华泰" && len(e.Sales) != 1 {
			t.Errorf("重开后明细 = %d 笔, want 1（不能重复迁移）", len(e.Sales))
		}
	}
}

func TestExportImport(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	b := newBroker(t, s, "华泰证券", "主账户")
	if _, err := s.SaveEntry(ctx, ledger.Entry{
		BrokerID: b.ID, FxCode: "920306", Name: "天元重工", IssuePriceFen: 448,
		AllottedShares: 500, CapitalCostFen: 95,
		Sales: []ledger.Sale{
			{Date: "2026-10-29", Shares: 200, PriceFen: 900},
			{Date: "2026-10-30", Shares: 300, PriceFen: 920, FeeFen: 500},
		},
	}); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	if err := s.SaveIssues(ctx, []source.Issue{{FxCode: "920306", Name: "天元重工", PriceFen: 448}}, "bse"); err != nil {
		t.Fatalf("SaveIssues: %v", err)
	}

	backup, err := s.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(backup.Brokers) != 1 {
		t.Fatalf("backup brokers = %d, want 1", len(backup.Brokers))
	}
	payload, err := json.Marshal(backup)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	fresh := newTestStore(t)
	count, err := fresh.Import(ctx, payload)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	entries, err := fresh.ListEntries(ctx)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].AllottedShares != 500 || entries[0].CapitalCostFen != 95 {
		t.Fatalf("entries = %+v", entries)
	}
	brokers, err := fresh.ListBrokers(ctx)
	if err != nil {
		t.Fatalf("ListBrokers: %v", err)
	}
	if len(brokers) != 1 || brokers[0].AnnualRatePPM != 38500 {
		t.Fatalf("brokers = %+v", brokers)
	}
	// 关联关系保留：导入后记录仍指向新库里的那个账户
	if entries[0].BrokerID != brokers[0].ID {
		t.Errorf("BrokerID = %d, want %d", entries[0].BrokerID, brokers[0].ID)
	}
	// 明细也要原样往返：两笔各带日期、股数、金额与费用。
	if len(entries[0].Sales) != 2 {
		t.Fatalf("导入后卖出明细 = %d 笔, want 2", len(entries[0].Sales))
	}
	if got, want := entries[0].Compute().SellAmountFen, int64(200*900+300*920); got != want {
		t.Errorf("成交金额 = %d, want %d", got, want)
	}
	if entries[0].Sales[1].FeeFen != 500 {
		t.Errorf("第二笔费用 = %d, want 500", entries[0].Sales[1].FeeFen)
	}
}

// TestImportLegacySingleSaleBackup 检查旧备份里「一笔合计」的卖出能恢复成明细。
func TestImportLegacySingleSaleBackup(t *testing.T) {
	s := newTestStore(t)
	payload, err := json.Marshal(Backup{
		App:     backupApp,
		Version: smallBackupVersion,
		Brokers: []ledger.Broker{{ID: 1, Name: "华泰证券"}},
		Entries: []backupEntry{{
			Entry:               ledger.Entry{ID: 7, BrokerID: 1, FxCode: "920306", AllottedShares: 500},
			LegacySoldShares:    500,
			LegacySellPriceFen:  920,
			LegacySellAmountFen: 460000,
			LegacySellFeeFen:    500,
			LegacySellDate:      "2026-10-30",
		}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := s.Import(context.Background(), payload); err != nil {
		t.Fatalf("Import: %v", err)
	}
	entries, err := s.ListEntries(context.Background())
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 || len(entries[0].Sales) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	sale := entries[0].Sales[0]
	if sale.Shares != 500 || sale.PriceFen != 920 || sale.AmountFen != 460000 ||
		sale.FeeFen != 500 || sale.Date != "2026-10-30" {
		t.Errorf("补出的明细 = %+v", sale)
	}
}

// TestImportLegacyBackup 检查不含券商账户的旧备份仍能导入。
func TestImportLegacyBackup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	payload, err := json.Marshal(Backup{
		App:     backupApp,
		Version: legacyBackupVersion,
		Entries: []backupEntry{{Entry: ledger.Entry{FxCode: "920002", Account: "老账户", IssuePriceFen: 100}}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := s.Import(ctx, payload); err != nil {
		t.Fatalf("Import: %v", err)
	}
	entries, err := s.ListEntries(ctx)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Account != "老账户" || entries[0].BrokerID == 0 {
		t.Fatalf("entries = %+v", entries)
	}
	brokers, err := s.ListBrokers(ctx)
	if err != nil {
		t.Fatalf("ListBrokers: %v", err)
	}
	if len(brokers) != 1 || brokers[0].Name != "老账户" {
		t.Fatalf("brokers = %+v", brokers)
	}
}

func TestImportRejectsForeignFile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Import(ctx, []byte(`{"app":"other","version":2}`)); err == nil {
		t.Fatal("should reject foreign backup")
	}
	if _, err := s.Import(ctx, []byte(`not json`)); err == nil {
		t.Fatal("should reject invalid json")
	}
}

func TestImportReplacesEntries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	b := newBroker(t, s, "华泰证券", "主账户")
	if _, err := s.SaveEntry(ctx, ledger.Entry{BrokerID: b.ID, FxCode: "920001"}); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	backup, err := json.Marshal(Backup{
		App: backupApp, Version: backupVersion,
		Brokers: []ledger.Broker{{ID: 9, Name: "B券商"}},
		Entries: []backupEntry{{Entry: ledger.Entry{BrokerID: 9, FxCode: "920002", Account: "B券商", IssuePriceFen: 100}}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := s.Import(ctx, backup); err != nil {
		t.Fatalf("Import: %v", err)
	}
	entries, err := s.ListEntries(ctx)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].FxCode != "920002" {
		t.Fatalf("entries = %+v", entries)
	}
	brokers, err := s.ListBrokers(ctx)
	if err != nil {
		t.Fatalf("ListBrokers: %v", err)
	}
	if len(brokers) != 1 || brokers[0].Name != "B券商" {
		t.Fatalf("旧账户应被替换，brokers = %+v", brokers)
	}
}

// TestListEntriesSortedByIssueDate 检查列表按新股发行顺序（申购日）排列。
//
// 改一条旧记录的备注不应该让它跳到列表顶部：排序看的是发行日，不是更新时间。
func TestListEntriesSortedByIssueDate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	broker := newBroker(t, s, "华泰证券", "尾号4169")

	mk := func(fxCode, name, purchaseDate string) ledger.Entry {
		t.Helper()
		saved, err := s.SaveEntry(ctx, ledger.Entry{
			BrokerID: broker.ID, FxCode: fxCode, Name: name,
			IssuePriceFen: 500, ApplyShares: 1000, PurchaseDate: purchaseDate,
		})
		if err != nil {
			t.Fatalf("SaveEntry %s: %v", fxCode, err)
		}
		return saved
	}
	early := mk("920001", "早发行", "2026-09-10")
	mk("920003", "晚发行", "2026-10-20")
	mk("920010", "同日一", "2026-10-20")
	mk("920011", "同日二", "2026-10-20")
	mk("920020", "没写申购日", "")

	// 改最早那条，它不该因为更新时间变新就跳到最上面。
	early.Note = "补一条备注"
	if _, err := s.SaveEntry(ctx, early); err != nil {
		t.Fatalf("update early: %v", err)
	}

	list, err := s.ListEntries(ctx)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	got := make([]string, len(list))
	for i, e := range list {
		got[i] = e.FxCode
	}
	// 最近发行的在前；同日按代码从大到小；没写申购日的垫底。
	want := []string{"920011", "920010", "920003", "920001", "920020"}
	if len(got) != len(want) {
		t.Fatalf("条数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v, want %v", got, want)
		}
	}
}
