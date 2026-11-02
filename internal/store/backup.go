package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wowh/ipo-book/internal/ledger"
	"github.com/wowh/ipo-book/internal/source"
)

// backupVersion 是备份文件的格式版本。
//
// 版本 3 起卖出是明细数组（sales），每笔各有成交日、股数、金额与费用；
// 版本 2 只有一组合计字段，版本 1 连券商账户都没有。两种旧版本读进来时按
// 记录里的合计字段补出一笔成交，因此老备份仍然可以导入。
const backupVersion = 3

// legacyBackupVersion 是不含券商账户的旧格式。
const legacyBackupVersion = 1

// smallBackupVersion 是只有一组卖出台计字段的旧格式（版本 2）。
const smallBackupVersion = 2

// backupApp 用于识别本应用的备份文件，防止误导入其他 JSON。
const backupApp = "bse-ipo-book"

// Backup 是账本备份文件的内容。
type Backup struct {
	App        string          `json:"app"`
	Version    int             `json:"version"`
	ExportedAt string          `json:"exportedAt"`
	Brokers    []ledger.Broker `json:"brokers"`
	Entries    []backupEntry   `json:"entries"`
	Issues     []source.Issue  `json:"issues"`
}

// backupEntry 是备份里的一条记录。
//
// 嵌入 ledger.Entry 之后，另外声明旧版本的卖出字段：那些版本只有一笔合计，
// 没有明细。读旧备份时用它们补出一笔成交，写新备份时不再输出。
type backupEntry struct {
	ledger.Entry
	LegacySoldShares    int64  `json:"soldShares,omitempty"`
	LegacySellPriceFen  int64  `json:"sellPriceFen,omitempty"`
	LegacySellAmountFen int64  `json:"sellAmountFen,omitempty"`
	LegacySellFeeFen    int64  `json:"sellFeeFen,omitempty"`
	LegacySellDate      string `json:"sellDate,omitempty"`
}

// Export 导出全部记录、券商账户与新股缓存。
func (s *Store) Export(ctx context.Context) (Backup, error) {
	entries, err := s.ListEntries(ctx)
	if err != nil {
		return Backup{}, err
	}
	brokers, err := s.ListBrokers(ctx)
	if err != nil {
		return Backup{}, err
	}
	issues, err := s.ListIssues(ctx)
	if err != nil {
		return Backup{}, err
	}
	items := make([]backupEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, backupEntry{Entry: e})
	}
	return Backup{
		App:        backupApp,
		Version:    backupVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Brokers:    brokers,
		Entries:    items,
		Issues:     issues,
	}, nil
}

// ErrInvalidBackup 表示文件不是本应用的有效备份。
var ErrInvalidBackup = errors.New("备份文件格式不正确或版本不受支持")

// Import 用备份内容替换账本记录，并合并新股缓存。
//
// 替换在单个事务内完成：任何一条记录校验失败都会整体回滚，不会留下半份账本。
// 导入前请先用 Export 保存当前账本，覆盖是不可撤销的。
func (s *Store) Import(ctx context.Context, payload []byte) (int, error) {
	var backup Backup
	if err := json.Unmarshal(payload, &backup); err != nil {
		return 0, fmt.Errorf("%w：%v", ErrInvalidBackup, err)
	}
	validVersion := backup.Version == backupVersion ||
		backup.Version == smallBackupVersion ||
		backup.Version == legacyBackupVersion
	if backup.App != backupApp || !validVersion {
		return 0, ErrInvalidBackup
	}
	for i := range backup.Brokers {
		if err := backup.Brokers[i].Validate(); err != nil {
			return 0, fmt.Errorf("第 %d 个券商账户无效：%w", i+1, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM entry_sales`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM brokers`); err != nil {
		return 0, err
	}

	stamp := now()
	// 备份里的账户 ID 只对原库有意义，导入后重新分配，并用「券商 + 账户」做映射。
	brokerIDs := make(map[int64]int64, len(backup.Brokers))
	keyIDs := make(map[string]int64, len(backup.Brokers))
	insertBroker := func(b ledger.Broker) (int64, error) {
		res, err := tx.ExecContext(ctx, `
INSERT INTO brokers (name, account, commission_ppm, min_commission_fen, annual_rate_ppm,
	note, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name, account) DO UPDATE SET updated_at = excluded.updated_at`,
			b.Name, b.Account, b.CommissionPPM, b.MinCommissionFen, b.AnnualRatePPM,
			b.Note, stamp, stamp)
		if err != nil {
			return 0, err
		}
		if id, err := res.LastInsertId(); err == nil && id > 0 {
			return id, nil
		}
		// 冲突时拿到已存在的 ID。
		var id int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM brokers WHERE name = ? AND account = ?`, b.Name, b.Account).Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}

	for _, b := range backup.Brokers {
		id, err := insertBroker(b)
		if err != nil {
			return 0, err
		}
		brokerIDs[b.ID] = id
		keyIDs[strings.TrimSpace(b.Name)+"\x00"+strings.TrimSpace(b.Account)] = id
	}

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO entries (broker_id, fx_code, stock_code, name, account, issue_price_fen,
	purchase_date, apply_shares, allotted_shares, allot_price_fen, freeze_date, refund_date,
	annual_rate_ppm, commission_ppm, min_commission_fen, other_fee_fen,
	capital_cost_fen, note, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, item := range backup.Entries {
		e := item.Entry
		if len(e.Sales) == 0 && item.LegacySoldShares > 0 {
			// 旧版本备份只有合计：折算成一笔成交，日期与金额照搬。
			e.Sales = []ledger.Sale{{
				Date:      item.LegacySellDate,
				Shares:    item.LegacySoldShares,
				PriceFen:  item.LegacySellPriceFen,
				AmountFen: item.LegacySellAmountFen,
				FeeFen:    item.LegacySellFeeFen,
			}}
		}
		brokerID := brokerIDs[e.BrokerID]
		if brokerID == 0 {
			// 旧格式备份没有账户 ID，按记录里的账户名建一个账户。
			name := strings.TrimSpace(e.Account)
			if name == "" {
				name = "默认账户"
			}
			key := name + "\x00"
			id, ok := keyIDs[key]
			if !ok {
				id, err = insertBroker(ledger.Broker{Name: name})
				if err != nil {
					return 0, err
				}
				keyIDs[key] = id
			}
			brokerID = id
			e.Account = name
		}
		e.BrokerID = brokerID
		if err := e.Validate(); err != nil {
			return 0, fmt.Errorf("记录 %s 无效：%w", e.FxCode, err)
		}
		created := e.CreatedAt
		if created == "" {
			created = stamp
		}
		updated := e.UpdatedAt
		if updated == "" {
			updated = stamp
		}
		res, err := stmt.ExecContext(ctx, brokerID, e.FxCode, e.StockCode, e.Name, e.Account,
			e.IssuePriceFen, e.PurchaseDate, e.ApplyShares, e.AllottedShares, e.AllotPriceFen,
			e.FreezeDate, e.RefundDate, e.AnnualRatePPM, e.CommissionPPM, e.MinCommissionFen,
			e.OtherFeeFen, e.CapitalCostFen, e.Note, created, updated)
		if err != nil {
			if isUniqueViolation(err) {
				return 0, fmt.Errorf("备份里同一账户重复出现 %s，请检查后再导入", e.FxCode)
			}
			return 0, err
		}
		entryID, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		if err := replaceSales(ctx, tx, entryID, e.Sales, stamp); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(backup.Entries), nil
}
