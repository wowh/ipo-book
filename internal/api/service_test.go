package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wowh/ipo-book/internal/ledger"
	"github.com/wowh/ipo-book/internal/source"
	"github.com/wowh/ipo-book/internal/store"
)

// fakeProvider 是可控的数据源，用于测试主备切换与写入流程。
type fakeProvider struct {
	id     string
	issues []source.Issue
	err    error
	calls  int
}

func (f *fakeProvider) ID() string    { return f.id }
func (f *fakeProvider) Label() string { return "模拟源" + f.id }

func (f *fakeProvider) Fetch(context.Context, source.Scope) ([]source.Issue, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.issues, nil
}

func newTestService(t *testing.T, providers ...source.Provider) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, source.NewManager(providers...))
}

func sampleIssues() []source.Issue {
	return []source.Issue{
		{FxCode: "920306", StockCode: "874158", Name: "天元重工", PriceFen: 448,
			PurchaseDate: "2026-10-20", ResultDate: "2026-10-23"},
		{FxCode: "920285", StockCode: "874235", Name: "远大健科", PriceFen: 2030,
			PurchaseDate: "2026-10-14", ListingDate: "2026-10-23"},
	}
}

func TestRefreshAndSnapshot(t *testing.T) {
	primary := &fakeProvider{id: "main", issues: sampleIssues()}
	backup := &fakeProvider{id: "backup", issues: []source.Issue{{FxCode: "920001", Name: "备用"}}}
	svc := newTestService(t, primary, backup)
	ctx := context.Background()

	result, err := svc.Refresh(ctx, "all")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if result.Count != 2 || result.Added != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Meta.Fallback {
		t.Error("should not fall back when primary works")
	}
	if backup.calls != 0 {
		t.Errorf("backup calls = %d, want 0", backup.calls)
	}

	snap, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Issues) != 2 {
		t.Fatalf("issues = %d, want 2", len(snap.Issues))
	}
	if snap.Meta.DataPath == "" {
		t.Error("DataPath should be set")
	}
	// 列表按申购日倒序，920306 申购日更晚
	if snap.Issues[0].FxCode != "920306" {
		t.Errorf("first issue = %s, want 920306", snap.Issues[0].FxCode)
	}
}

func TestFallbackToBackupSource(t *testing.T) {
	primary := &fakeProvider{id: "main", err: errors.New("主源连不上")}
	backup := &fakeProvider{id: "backup", issues: sampleIssues()}
	svc := newTestService(t, primary, backup)
	ctx := context.Background()

	result, err := svc.Refresh(ctx, "all")
	if err != nil {
		t.Fatalf("Refresh should succeed via backup: %v", err)
	}
	if !result.Meta.Fallback {
		t.Fatal("Fallback should be true")
	}
	if result.Meta.Warning == "" {
		t.Error("Warning should explain the fallback")
	}
	if result.Meta.SourceLabel != "模拟源backup" {
		t.Errorf("SourceLabel = %q", result.Meta.SourceLabel)
	}
}

func TestRefreshFailsWhenAllSourcesFail(t *testing.T) {
	svc := newTestService(t,
		&fakeProvider{id: "main", err: errors.New("网络错误")},
		&fakeProvider{id: "backup", err: errors.New("网络错误")},
	)
	if _, err := svc.Refresh(context.Background(), "all"); err == nil {
		t.Fatal("expected error when every source fails")
	}
}

// newBroker 通过服务层建一个券商账户。
func newBroker(t *testing.T, svc *Service) ledger.Broker {
	t.Helper()
	b, err := svc.SaveBroker(context.Background(), ledger.Broker{
		Name: "华泰证券", Account: "主账户",
		CommissionPPM: 250, MinCommissionFen: 500, AnnualRatePPM: 38500,
	})
	if err != nil {
		t.Fatalf("SaveBroker: %v", err)
	}
	return b
}

func TestSaveEntryAndSummary(t *testing.T) {
	svc := newTestService(t, &fakeProvider{id: "main", issues: sampleIssues()})
	ctx := context.Background()
	if _, err := svc.Refresh(ctx, "all"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	broker := newBroker(t, svc)

	view, err := svc.SaveEntry(ctx, ledger.Entry{
		BrokerID: broker.ID, FxCode: "920306", Name: "天元重工",
		IssuePriceFen: 448, PurchaseDate: "2026-10-20", RefundDate: "2026-10-22",
		ApplyShares: 1000, AllottedShares: 500,
		Sales: []ledger.Sale{{Date: "2026-10-30", Shares: 500, PriceFen: 900}},
	})
	if err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	if view.ID == 0 {
		t.Fatal("ID should be assigned")
	}
	// 账户名称与费率由服务端按券商账户补齐
	if view.Account != "华泰证券 · 主账户" {
		t.Errorf("Account = %q", view.Account)
	}
	if view.AnnualRatePPM != 38500 || view.CommissionPPM != 250 {
		t.Errorf("费率快照 = %+v", view.Entry)
	}
	// 资金成本 448000×38500×2÷(1e6×365)=95；卖出佣金按最低 500
	// 收入 500*900=450000，成本 224000，支出 595 → 225405
	if view.CapitalCostFen != 95 || view.SellFeeFen != 500 {
		t.Errorf("cost = %d fee = %d", view.CapitalCostFen, view.SellFeeFen)
	}
	if view.RealizedFen != 225405 {
		t.Errorf("RealizedFen = %d, want 225405", view.RealizedFen)
	}

	snap, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Summary.EntryCount != 1 || snap.Summary.RealizedFen != 225405 {
		t.Fatalf("summary = %+v", snap.Summary)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Status != ledger.StatusSold {
		t.Fatalf("entries = %+v", snap.Entries)
	}
	if len(snap.Brokers) != 1 || snap.Brokers[0].ID != broker.ID {
		t.Fatalf("brokers = %+v", snap.Brokers)
	}
	if len(snap.Summary.ByAccount) != 1 || snap.Summary.ByAccount[0].CapitalCostFen != 95 {
		t.Fatalf("byAccount = %+v", snap.Summary.ByAccount)
	}
}

func TestSaveEntryDuplicateMessage(t *testing.T) {
	svc := newTestService(t, &fakeProvider{id: "main", issues: sampleIssues()})
	ctx := context.Background()
	broker := newBroker(t, svc)
	entry := ledger.Entry{BrokerID: broker.ID, FxCode: "920306", Name: "天元重工", IssuePriceFen: 448}
	if _, err := svc.SaveEntry(ctx, entry); err != nil {
		t.Fatalf("first save: %v", err)
	}
	_, err := svc.SaveEntry(ctx, entry)
	if err == nil {
		t.Fatal("duplicate should fail")
	}
	if !strings.Contains(err.Error(), "已有记录") {
		t.Errorf("error should explain the duplicate: %v", err)
	}
}

func TestSaveEntryRequiresBroker(t *testing.T) {
	svc := newTestService(t, &fakeProvider{id: "main", issues: sampleIssues()})
	ctx := context.Background()
	_, err := svc.SaveEntry(ctx, ledger.Entry{FxCode: "920306", Name: "天元重工", IssuePriceFen: 448})
	if !errors.Is(err, ledger.ErrMissingAccount) {
		t.Fatalf("err = %v, want ErrMissingAccount", err)
	}
}

func TestExportImportThroughService(t *testing.T) {
	svc := newTestService(t, &fakeProvider{id: "main", issues: sampleIssues()})
	ctx := context.Background()
	broker := newBroker(t, svc)
	if _, err := svc.SaveEntry(ctx, ledger.Entry{BrokerID: broker.ID, FxCode: "920306", Name: "天元重工", IssuePriceFen: 448}); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	payload, err := svc.ExportJSON(ctx)
	if err != nil {
		t.Fatalf("ExportJSON: %v", err)
	}
	if payload == "" {
		t.Fatal("payload should not be empty")
	}

	other := newTestService(t, &fakeProvider{id: "main"})
	count, err := other.ImportJSON(ctx, payload)
	if err != nil {
		t.Fatalf("ImportJSON: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if _, err := other.ImportJSON(ctx, "{}"); err == nil {
		t.Fatal("invalid payload should be rejected")
	}
}

func TestBrokerCRUD(t *testing.T) {
	svc := newTestService(t, &fakeProvider{id: "main", issues: sampleIssues()})
	ctx := context.Background()

	ht, err := svc.SaveBroker(ctx, ledger.Broker{Name: "华泰证券", Account: "主账户", AnnualRatePPM: 38500})
	if err != nil {
		t.Fatalf("SaveBroker: %v", err)
	}
	citic, err := svc.SaveBroker(ctx, ledger.Broker{Name: "中信证券", Account: "主账户"})
	if err != nil {
		t.Fatalf("SaveBroker: %v", err)
	}

	// 同名券商同账户不能重复
	if _, err := svc.SaveBroker(ctx, ledger.Broker{Name: "华泰证券", Account: "主账户"}); err == nil {
		t.Fatal("duplicate broker should fail")
	}
	// 缺少券商名称
	if _, err := svc.SaveBroker(ctx, ledger.Broker{Account: "无券商"}); err == nil {
		t.Fatal("broker without name should fail")
	}

	snap, err := svc.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Brokers) != 2 {
		t.Fatalf("brokers = %d, want 2", len(snap.Brokers))
	}

	// 有记录的账户不能删
	if _, err := svc.SaveEntry(ctx, ledger.Entry{BrokerID: ht.ID, FxCode: "920001", IssuePriceFen: 100}); err != nil {
		t.Fatalf("SaveEntry: %v", err)
	}
	if err := svc.DeleteBroker(ctx, ht.ID); err == nil {
		t.Fatal("broker in use should not be deletable")
	}
	if err := svc.DeleteBroker(ctx, citic.ID); err != nil {
		t.Fatalf("DeleteBroker: %v", err)
	}
}
