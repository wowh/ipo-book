// Package store 用 SQLite 保存新股缓存、券商账户与打新账本。
//
// 数据库是纯 Go 实现（modernc.org/sqlite），不依赖 cgo，因此同一个二进制可以
// 交叉编译到 macOS、Windows 和 Linux。数据全部留在本地文件里。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wowh/ipo-book/internal/ledger"
	"github.com/wowh/ipo-book/internal/source"
)

// schema 是建表语句。新增字段时同步更新 migrate 里的补列逻辑。
const schema = `
CREATE TABLE IF NOT EXISTS issues (
	fx_code       TEXT PRIMARY KEY,
	stock_code    TEXT NOT NULL DEFAULT '',
	name          TEXT NOT NULL DEFAULT '',
	price_fen     INTEGER NOT NULL DEFAULT 0,
	issue_shares  INTEGER NOT NULL DEFAULT 0,
	pe_hundredths INTEGER NOT NULL DEFAULT 0,
	purchase_date TEXT NOT NULL DEFAULT '',
	result_date   TEXT NOT NULL DEFAULT '',
	listing_date  TEXT NOT NULL DEFAULT '',
	source        TEXT NOT NULL DEFAULT '',
	updated_at    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS brokers (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	name               TEXT NOT NULL DEFAULT '',
	account            TEXT NOT NULL DEFAULT '',
	commission_ppm     INTEGER NOT NULL DEFAULT 0,
	min_commission_fen INTEGER NOT NULL DEFAULT 0,
	annual_rate_ppm    INTEGER NOT NULL DEFAULT 0,
	note               TEXT NOT NULL DEFAULT '',
	created_at         TEXT NOT NULL DEFAULT '',
	updated_at         TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS brokers_name_account
	ON brokers (name, account);

CREATE TABLE IF NOT EXISTS entries (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	broker_id          INTEGER NOT NULL DEFAULT 0,
	fx_code            TEXT NOT NULL,
	stock_code         TEXT NOT NULL DEFAULT '',
	name               TEXT NOT NULL DEFAULT '',
	account            TEXT NOT NULL DEFAULT '',
	issue_price_fen    INTEGER NOT NULL DEFAULT 0,
	purchase_date      TEXT NOT NULL DEFAULT '',
	apply_shares       INTEGER NOT NULL DEFAULT 0,
	allotted_shares    INTEGER NOT NULL DEFAULT 0,
	allot_price_fen    INTEGER NOT NULL DEFAULT 0,
	freeze_date        TEXT NOT NULL DEFAULT '',
	refund_date        TEXT NOT NULL DEFAULT '',
	annual_rate_ppm    INTEGER NOT NULL DEFAULT 0,
	commission_ppm     INTEGER NOT NULL DEFAULT 0,
	min_commission_fen INTEGER NOT NULL DEFAULT 0,
	fee_fen            INTEGER NOT NULL DEFAULT 0,
	other_fee_fen      INTEGER NOT NULL DEFAULT 0,
	capital_cost_fen   INTEGER NOT NULL DEFAULT 0,
	sell_price_fen     INTEGER NOT NULL DEFAULT 0,
	sell_amount_fen    INTEGER NOT NULL DEFAULT 0,
	sold_shares        INTEGER NOT NULL DEFAULT 0,
	sell_date          TEXT NOT NULL DEFAULT '',
	note               TEXT NOT NULL DEFAULT '',
	created_at         TEXT NOT NULL DEFAULT '',
	updated_at         TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS entry_sales (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	entry_id   INTEGER NOT NULL,
	sale_date  TEXT NOT NULL DEFAULT '',
	shares     INTEGER NOT NULL DEFAULT 0,
	price_fen  INTEGER NOT NULL DEFAULT 0,
	amount_fen INTEGER NOT NULL DEFAULT 0,
	fee_fen    INTEGER NOT NULL DEFAULT 0,
	note       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS entry_sales_entry ON entry_sales (entry_id, sale_date, id);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`

// entryColumns 是账本表在历次版本里新增的列。
//
// 老版本的数据库文件没有这些列，Open 时按需补上，让升级不影响已有账本。
var entryColumns = []struct {
	name string
	ddl  string
}{
	{"broker_id", "INTEGER NOT NULL DEFAULT 0"},
	{"purchase_date", "TEXT NOT NULL DEFAULT ''"},
	{"freeze_date", "TEXT NOT NULL DEFAULT ''"},
	{"refund_date", "TEXT NOT NULL DEFAULT ''"},
	{"annual_rate_ppm", "INTEGER NOT NULL DEFAULT 0"},
	{"commission_ppm", "INTEGER NOT NULL DEFAULT 0"},
	{"min_commission_fen", "INTEGER NOT NULL DEFAULT 0"},
	{"other_fee_fen", "INTEGER NOT NULL DEFAULT 0"},
	{"capital_cost_fen", "INTEGER NOT NULL DEFAULT 0"},
	{"sell_amount_fen", "INTEGER NOT NULL DEFAULT 0"},
}

// Store 是账本数据库。
type Store struct {
	db   *sql.DB
	path string
}

// Path 返回数据库文件的位置。
func (s *Store) Path() string { return s.path }

// Open 打开（必要时创建）指定路径的数据库。
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("数据库路径为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败：%w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败：%w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库失败：%w", err)
	}
	st := &Store{db: db, path: path}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级数据库失败：%w", err)
	}
	return st, nil
}

// migrate 把旧版本的账本补到当前结构。
//
// 关键一步是把历史上直接填在记录里的账户名，转成正式的券商账户：同名的账户
// 合并成一个，记录通过 broker_id 关联过去，账户名称仍然留在记录里作为留痕。
func (s *Store) migrate() error {
	columns, err := s.entryColumns()
	if err != nil {
		return err
	}
	for _, col := range entryColumns {
		if columns[col.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE entries ADD COLUMN ` + col.name + " " + col.ddl); err != nil {
			return err
		}
	}

	if err := s.adoptLegacyAccounts(); err != nil {
		return err
	}
	if err := s.migrateSales(); err != nil {
		return err
	}
	return s.ensureIndexes()
}

// migrateSales 把旧版本里的单笔卖出转成一条成交明细。
//
// 旧结构只有一组「卖出股数 / 均价 / 成交金额 / 费用 / 日期」，记不下分几笔卖。
// 这里把它转成一行明细，然后把旧列清零：旧列留着会和新明细构成两份真相，
// 谁也不敢判断哪个对。
//
// 只在「有卖出股数且还没有明细」时迁移，因此重复启动不会产生重复成交。
func (s *Store) migrateSales() error {
	type legacy struct {
		id                            int64
		date                          string
		shares, price, amount, feeFen int64
	}
	rows, err := s.db.Query(`
SELECT id, sell_date, sold_shares, sell_price_fen, sell_amount_fen, fee_fen
FROM entries
WHERE sold_shares > 0`)
	if err != nil {
		return err
	}
	var pending []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.id, &l.date, &l.shares, &l.price, &l.amount, &l.feeFen); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now()
	for _, l := range pending {
		var has int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM entry_sales WHERE entry_id = ?`, l.id).Scan(&has); err != nil {
			return err
		}
		if has > 0 {
			continue
		}
		if _, err := tx.Exec(`
INSERT INTO entry_sales (entry_id, sale_date, shares, price_fen, amount_fen, fee_fen, note, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)`,
			l.id, l.date, l.shares, l.price, l.amount, l.feeFen, stamp, stamp); err != nil {
			return err
		}
	}
	// 迁移过的行把旧列清零，下次启动就不再被当成待迁移。
	if _, err := tx.Exec(`
UPDATE entries SET sold_shares = 0, sell_price_fen = 0, sell_amount_fen = 0,
	fee_fen = 0, sell_date = ''
WHERE sold_shares > 0`); err != nil {
		return err
	}
	return tx.Commit()
}

// adoptLegacyAccounts 把历史上直接填在记录里的账户名转成券商账户。
func (s *Store) adoptLegacyAccounts() error {
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM entries WHERE broker_id = 0`).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT DISTINCT account FROM entries WHERE broker_id = 0`)
	if err != nil {
		return err
	}
	names := make([]string, 0, 8)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	stamp := now()
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			name = "默认账户"
		}
		if _, err := tx.Exec(`
INSERT INTO brokers (name, account, created_at, updated_at) VALUES (?, '', ?, ?)
ON CONFLICT(name, account) DO NOTHING`, name, stamp, stamp); err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(`SELECT id FROM brokers WHERE name = ? AND account = ''`, name).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE entries SET account = ?, broker_id = ? WHERE broker_id = 0 AND (account = ? OR (account = '' AND ? = '默认账户'))`,
			name, id, name, name); err != nil {
			return err
		}
	}
	// 剩下的空账户统一归到默认账户。
	if _, err := tx.Exec(`
INSERT INTO brokers (name, account, created_at, updated_at) VALUES ('默认账户', '', ?, ?)
ON CONFLICT(name, account) DO NOTHING`, stamp, stamp); err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE entries SET account = '默认账户', broker_id = (SELECT id FROM brokers WHERE name = '默认账户' AND account = '')
WHERE broker_id = 0`); err != nil {
		return err
	}
	return tx.Commit()
}

// ensureIndexes 建立依赖新增列的唯一索引。
//
// 唯一索引引用了 broker_id，而老库要先补列才能建索引，所以放在补列之后执行；
// 每次打开都执行一遍，新建的库也因此拿到同样的约束。
func (s *Store) ensureIndexes() error {
	if _, err := s.db.Exec(`
CREATE UNIQUE INDEX IF NOT EXISTS entries_broker_issue
	ON entries (fx_code, broker_id)`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DROP INDEX IF EXISTS entries_account_issue`)
	return err
}

// entryColumns 返回账本表当前已有的列名集合。
func (s *Store) entryColumns() (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(entries)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool, 24)
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// now 返回统一的 UTC 时间戳文本。
func now() string { return time.Now().UTC().Format(time.RFC3339) }

// SaveIssues 把一次抓取到的新股写入缓存。已存在的记录被最新数据覆盖，
// 但不会因为本次未出现而删除，避免离线时列表变空。
func (s *Store) SaveIssues(ctx context.Context, issues []source.Issue, sourceID string) error {
	if len(issues) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO issues (fx_code, stock_code, name, price_fen, issue_shares, pe_hundredths,
	purchase_date, result_date, listing_date, source, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(fx_code) DO UPDATE SET
	stock_code    = excluded.stock_code,
	name          = excluded.name,
	price_fen     = CASE WHEN excluded.price_fen > 0 THEN excluded.price_fen ELSE issues.price_fen END,
	issue_shares  = CASE WHEN excluded.issue_shares > 0 THEN excluded.issue_shares ELSE issues.issue_shares END,
	pe_hundredths = CASE WHEN excluded.pe_hundredths > 0 THEN excluded.pe_hundredths ELSE issues.pe_hundredths END,
	purchase_date = CASE WHEN excluded.purchase_date <> '' THEN excluded.purchase_date ELSE issues.purchase_date END,
	result_date   = CASE WHEN excluded.result_date <> '' THEN excluded.result_date ELSE issues.result_date END,
	listing_date  = CASE WHEN excluded.listing_date <> '' THEN excluded.listing_date ELSE issues.listing_date END,
	source        = excluded.source,
	updated_at    = excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	stamp := now()
	for _, issue := range issues {
		if _, err := stmt.ExecContext(ctx, issue.FxCode, issue.StockCode, issue.Name,
			issue.PriceFen, issue.IssueShares, issue.PEHundredths,
			issue.PurchaseDate, issue.ResultDate, issue.ListingDate, sourceID, stamp); err != nil {
			return fmt.Errorf("写入新股 %s 失败：%w", issue.FxCode, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES ('issues_updated_at', ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

// ListIssues 返回缓存的新股，按申购日从新到旧排列。
func (s *Store) ListIssues(ctx context.Context) ([]source.Issue, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT fx_code, stock_code, name, price_fen, issue_shares, pe_hundredths,
       purchase_date, result_date, listing_date
FROM issues
ORDER BY purchase_date DESC, fx_code DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	issues := make([]source.Issue, 0, 64)
	for rows.Next() {
		var issue source.Issue
		if err := rows.Scan(&issue.FxCode, &issue.StockCode, &issue.Name, &issue.PriceFen,
			&issue.IssueShares, &issue.PEHundredths, &issue.PurchaseDate,
			&issue.ResultDate, &issue.ListingDate); err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// IssuesUpdatedAt 返回最近一次同步新股列表的时间，未同步过则为空。
func (s *Store) IssuesUpdatedAt(ctx context.Context) string {
	var value string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'issues_updated_at'`).Scan(&value); err != nil {
		return ""
	}
	return value
}

// ListBrokers 返回全部券商账户，按券商名称与账户排列。
func (s *Store) ListBrokers(ctx context.Context) ([]ledger.Broker, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, account, commission_ppm, min_commission_fen, annual_rate_ppm,
       note, created_at, updated_at
FROM brokers
ORDER BY name COLLATE NOCASE, account COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	brokers := make([]ledger.Broker, 0, 8)
	for rows.Next() {
		var b ledger.Broker
		if err := rows.Scan(&b.ID, &b.Name, &b.Account, &b.CommissionPPM, &b.MinCommissionFen,
			&b.AnnualRatePPM, &b.Note, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		brokers = append(brokers, b)
	}
	return brokers, rows.Err()
}

// SaveBroker 新增或更新一个券商账户。
//
// 同一个「券商 + 账户」只允许一条；改费率不会改写已有记录，因为每条记录都保存
// 了当时的费率快照，历史成本不随参数变化。
func (s *Store) SaveBroker(ctx context.Context, b ledger.Broker) (ledger.Broker, error) {
	if err := b.Validate(); err != nil {
		return ledger.Broker{}, err
	}
	stamp := now()
	if b.ID == 0 {
		res, err := s.db.ExecContext(ctx, `
INSERT INTO brokers (name, account, commission_ppm, min_commission_fen, annual_rate_ppm,
	note, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			b.Name, b.Account, b.CommissionPPM, b.MinCommissionFen, b.AnnualRatePPM,
			b.Note, stamp, stamp)
		if err != nil {
			if isUniqueViolation(err) {
				return ledger.Broker{}, errors.New("这个券商账户已经存在")
			}
			return ledger.Broker{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return ledger.Broker{}, err
		}
		b.ID, b.CreatedAt, b.UpdatedAt = id, stamp, stamp
		return b, nil
	}

	res, err := s.db.ExecContext(ctx, `
UPDATE brokers SET name = ?, account = ?, commission_ppm = ?, min_commission_fen = ?,
	annual_rate_ppm = ?, note = ?, updated_at = ?
WHERE id = ?`,
		b.Name, b.Account, b.CommissionPPM, b.MinCommissionFen, b.AnnualRatePPM,
		b.Note, stamp, b.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ledger.Broker{}, errors.New("这个券商账户已经存在")
		}
		return ledger.Broker{}, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ledger.Broker{}, err
	}
	if affected == 0 {
		return ledger.Broker{}, errors.New("券商账户不存在或已被删除")
	}
	b.UpdatedAt = stamp
	if err := s.db.QueryRowContext(ctx,
		`SELECT created_at FROM brokers WHERE id = ?`, b.ID).Scan(&b.CreatedAt); err != nil {
		return ledger.Broker{}, err
	}
	// 记录里的账户名称是快照，改名时同步过去，避免列表与账户对不上。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE entries SET account = ? WHERE broker_id = ?`, b.Label(), b.ID); err != nil {
		return ledger.Broker{}, err
	}
	return b, nil
}

// DeleteBroker 删除券商账户。仍有记录在使用时拒绝删除，避免账本出现孤儿数据。
func (s *Store) DeleteBroker(ctx context.Context, id int64) error {
	var used int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entries WHERE broker_id = ?`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return fmt.Errorf("这个账户下还有 %d 条打新记录，先删除或改到别的账户再删", used)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM brokers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return errors.New("券商账户不存在或已被删除")
	}
	return nil
}

// ListEntries 返回全部打新记录，按新股发行顺序排列。
//
// 发行顺序就是申购日的先后，与「新股列表」同口径：最近发行的在最上面，同一天
// 发行的按代码排。没用更新时间，改一条记录不应该让它跳到列表顶部。
// 没填申购日的记录垫底（空串在 SQLite 里比任何日期都小，DESC 时排最后）。
func (s *Store) ListEntries(ctx context.Context) ([]ledger.Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, broker_id, fx_code, stock_code, name, account, issue_price_fen, purchase_date,
       apply_shares, allotted_shares, allot_price_fen, freeze_date, refund_date,
       annual_rate_ppm, commission_ppm, min_commission_fen, other_fee_fen,
       capital_cost_fen, note, created_at, updated_at
FROM entries
ORDER BY purchase_date DESC, fx_code DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]ledger.Entry, 0, 32)
	for rows.Next() {
		var e ledger.Entry
		if err := rows.Scan(&e.ID, &e.BrokerID, &e.FxCode, &e.StockCode, &e.Name, &e.Account,
			&e.IssuePriceFen, &e.PurchaseDate, &e.ApplyShares, &e.AllottedShares,
			&e.AllotPriceFen, &e.FreezeDate, &e.RefundDate, &e.AnnualRatePPM,
			&e.CommissionPPM, &e.MinCommissionFen, &e.OtherFeeFen,
			&e.CapitalCostFen, &e.Note, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.fillSales(ctx, entries); err != nil {
		return nil, err
	}
	return entries, s.fillAccountNames(ctx, entries)
}

// fillSales 一次性把所有成交明细挂到对应记录上。
//
// 按 entry_id 建索引再分派，避免每条记录查一次库；账本是几千条量级，全表扫
// 一遍的代价远小于 N 次往返。
func (s *Store) fillSales(ctx context.Context, entries []ledger.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	position := make(map[int64]int, len(entries))
	for i := range entries {
		position[entries[i].ID] = i
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT entry_id, id, sale_date, shares, price_fen, amount_fen, fee_fen, note
FROM entry_sales
ORDER BY entry_id, sale_date, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			entryID int64
			sale    ledger.Sale
		)
		if err := rows.Scan(&entryID, &sale.ID, &sale.Date, &sale.Shares, &sale.PriceFen,
			&sale.AmountFen, &sale.FeeFen, &sale.Note); err != nil {
			return err
		}
		if i, ok := position[entryID]; ok {
			entries[i].Sales = append(entries[i].Sales, sale)
		}
	}
	return rows.Err()
}

// replaceSales 用给定明细替换一条记录的全部卖出，并在事务内执行。
//
// 整条保存时明细是「全量提交」：界面上删掉一笔再保存，库里也就没有那一笔了。
// 但已有明细的 ID 必须留住——agent 会拿 saleId 定位某一笔去改或删，如果每次保存
// 都先全删再插，ID 会整体漂移，手里的 ID 立刻就失效了。
func replaceSales(ctx context.Context, tx *sql.Tx, entryID int64, sales []ledger.Sale, stamp string) error {
	existing := make(map[int64]bool)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM entry_sales WHERE entry_id = ?`, entryID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	update, err := tx.PrepareContext(ctx, `
UPDATE entry_sales SET sale_date = ?, shares = ?, price_fen = ?, amount_fen = ?, fee_fen = ?,
	note = ?, updated_at = ?
WHERE id = ? AND entry_id = ?`)
	if err != nil {
		return err
	}
	defer update.Close()
	insert, err := tx.PrepareContext(ctx, `
INSERT INTO entry_sales (entry_id, sale_date, shares, price_fen, amount_fen, fee_fen, note,
	created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()

	kept := make(map[int64]bool, len(sales))
	for i := range sales {
		sale := &sales[i]
		if sale.ID != 0 && existing[sale.ID] {
			if _, err := update.ExecContext(ctx, sale.Date, sale.Shares, sale.PriceFen,
				sale.AmountFen, sale.FeeFen, sale.Note, stamp, sale.ID, entryID); err != nil {
				return err
			}
			kept[sale.ID] = true
			continue
		}
		res, err := insert.ExecContext(ctx, entryID, sale.Date, sale.Shares, sale.PriceFen,
			sale.AmountFen, sale.FeeFen, sale.Note, stamp, stamp)
		if err != nil {
			return err
		}
		// 回填库里分配的 ID，调用方拿到的明细与下次读出来的完全一致。
		if id, err := res.LastInsertId(); err == nil {
			sale.ID = id
		}
	}

	// 提交上来的清单里已经消失的明细，才是真的被删掉的那几笔。
	for id := range existing {
		if kept[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM entry_sales WHERE id = ? AND entry_id = ?`,
			id, entryID); err != nil {
			return err
		}
	}
	return nil
}

// fillAccountNames 用券商账户补齐记录里缺失的账户名称。
//
// 正常情况下保存记录时就会写快照，这里兜住两种情况：手工导入的数据，
// 以及账户被改名后还没来得及同步的老记录。
func (s *Store) fillAccountNames(ctx context.Context, entries []ledger.Entry) error {
	need := false
	for _, e := range entries {
		if strings.TrimSpace(e.Account) == "" {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	brokers, err := s.ListBrokers(ctx)
	if err != nil {
		return err
	}
	labels := make(map[int64]string, len(brokers))
	for _, b := range brokers {
		labels[b.ID] = b.Label()
	}
	for i := range entries {
		if strings.TrimSpace(entries[i].Account) == "" {
			if label, ok := labels[entries[i].BrokerID]; ok {
				entries[i].Account = label
			}
		}
	}
	return nil
}

// SaveEntry 新增或更新一条记录。
//
// ID 为 0 时新增；同一账户下重复录入同一只新股会被拒绝。更新时保留创建时间。
// 记录与卖出明细在同一个事务里写，明细按提交的全量替换：删掉一笔再保存，
// 库里也就没有那一笔了。
func (s *Store) SaveEntry(ctx context.Context, e ledger.Entry) (ledger.Entry, error) {
	if err := e.Validate(); err != nil {
		return ledger.Entry{}, err
	}
	stamp := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ledger.Entry{}, err
	}
	defer tx.Rollback()

	if e.ID == 0 {
		res, err := tx.ExecContext(ctx, `
INSERT INTO entries (broker_id, fx_code, stock_code, name, account, issue_price_fen,
	purchase_date, apply_shares, allotted_shares, allot_price_fen, freeze_date, refund_date,
	annual_rate_ppm, commission_ppm, min_commission_fen, other_fee_fen,
	capital_cost_fen, note, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.BrokerID, e.FxCode, e.StockCode, e.Name, e.Account, e.IssuePriceFen,
			e.PurchaseDate, e.ApplyShares, e.AllottedShares, e.AllotPriceFen,
			e.FreezeDate, e.RefundDate, e.AnnualRatePPM, e.CommissionPPM, e.MinCommissionFen,
			e.OtherFeeFen, e.CapitalCostFen, e.Note, stamp, stamp)
		if err != nil {
			if isUniqueViolation(err) {
				return ledger.Entry{}, ledger.ErrDuplicateEntry
			}
			return ledger.Entry{}, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return ledger.Entry{}, err
		}
		e.ID = id
		e.CreatedAt, e.UpdatedAt = stamp, stamp
	} else {
		res, err := tx.ExecContext(ctx, `
UPDATE entries SET
	broker_id = ?, fx_code = ?, stock_code = ?, name = ?, account = ?, issue_price_fen = ?,
	purchase_date = ?, apply_shares = ?, allotted_shares = ?, allot_price_fen = ?,
	freeze_date = ?, refund_date = ?, annual_rate_ppm = ?, commission_ppm = ?,
	min_commission_fen = ?, other_fee_fen = ?, capital_cost_fen = ?, note = ?,
	updated_at = ?
WHERE id = ?`,
			e.BrokerID, e.FxCode, e.StockCode, e.Name, e.Account, e.IssuePriceFen,
			e.PurchaseDate, e.ApplyShares, e.AllottedShares, e.AllotPriceFen,
			e.FreezeDate, e.RefundDate, e.AnnualRatePPM, e.CommissionPPM, e.MinCommissionFen,
			e.OtherFeeFen, e.CapitalCostFen, e.Note, stamp, e.ID)
		if err != nil {
			if isUniqueViolation(err) {
				return ledger.Entry{}, ledger.ErrDuplicateEntry
			}
			return ledger.Entry{}, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return ledger.Entry{}, err
		}
		if affected == 0 {
			return ledger.Entry{}, fmt.Errorf("记录不存在或已被删除")
		}
		e.UpdatedAt = stamp
		if err := tx.QueryRowContext(ctx,
			`SELECT created_at FROM entries WHERE id = ?`, e.ID).Scan(&e.CreatedAt); err != nil {
			return ledger.Entry{}, err
		}
	}

	if err := replaceSales(ctx, tx, e.ID, e.Sales, stamp); err != nil {
		return ledger.Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return ledger.Entry{}, err
	}
	return e, nil
}

// DeleteEntry 删除一条记录及其卖出明细。
func (s *Store) DeleteEntry(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM entry_sales WHERE entry_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("记录不存在或已被删除")
	}
	return tx.Commit()
}

// isUniqueViolation 判断错误是否由唯一索引冲突引起。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
