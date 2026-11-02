// Package source 提供北交所新股发行数据。
//
// 主源为北交所官网 www.bse.cn 的新股发行列表接口；备用源为北交所信息披露
// 平台 www.bseinfo.net 的同名接口。两者由同一运营方发布、字段一致，主源不可
// 用时自动降级到备用源，并在结果中标注实际来源，便于用户判断数据时效。
package source

import (
	"context"
	"errors"
	"fmt"
)

// Scope 选择要拉取的发行阶段。
type Scope string

const (
	// ScopeIssuing 为正在发行（尚未公布中签结果）的新股。
	ScopeIssuing Scope = "issuing"
	// ScopeDone 为已完成发行的新股。
	ScopeDone Scope = "done"
	// ScopeAll 为全部新股。
	ScopeAll Scope = "all"
)

// Valid 报告 scope 是否为已知取值。
func (s Scope) Valid() bool {
	switch s {
	case ScopeIssuing, ScopeDone, ScopeAll:
		return true
	default:
		return false
	}
}

// statetype 把 scope 映射为北交所接口的 statetypes 参数。
func (s Scope) statetype() string {
	switch s {
	case ScopeIssuing:
		return "1"
	case ScopeDone:
		return "2"
	default:
		return ""
	}
}

// Issue 是一只北交所新股的发行信息。
type Issue struct {
	// FxCode 是申购代码（920xxx），与上市后的证券代码不同。
	FxCode string `json:"fxCode"`
	// StockCode 是挂牌/上市证券代码（8xxxxx）。
	StockCode string `json:"stockCode"`
	Name      string `json:"name"`
	// PriceFen 是发行价，以分为单位的整数。0 表示尚未定价。
	PriceFen int64 `json:"priceFen"`
	// IssueShares 是本次发行数量（股），0 表示未知。
	IssueShares int64 `json:"issueShares"`
	// PEHundredths 是发行市盈率乘以 100 的整数，0 表示未知。
	PEHundredths int64 `json:"peHundredths"`
	// PurchaseDate 是申购日，格式 YYYY-MM-DD，空表示未公布。
	PurchaseDate string `json:"purchaseDate"`
	// ResultDate 是中签结果公布日。
	ResultDate string `json:"resultDate"`
	// ListingDate 是上市日。
	ListingDate string `json:"listingDate"`
}

// Priced 报告该新股是否已公布发行价。
func (i Issue) Priced() bool { return i.PriceFen > 0 }

// Result 是一次列表拉取的结果。
type Result struct {
	Issues []Issue `json:"issues"`
	// Source 是实际提供数据的来源标识，如 "bse" 或 "bseinfo"。
	Source string `json:"source"`
	// SourceLabel 是来源的中文名称，供界面直接展示。
	SourceLabel string `json:"sourceLabel"`
	// Fallback 表示主源失败、已降级到备用源。
	Fallback bool `json:"fallback"`
	// Warning 是给用户看的降级说明，非降级时为空。
	Warning string `json:"warning"`
}

// Provider 是一个新股数据来源。
type Provider interface {
	// Label 返回来源的中文名称。
	Label() string
	// ID 返回来源标识。
	ID() string
	// Fetch 拉取指定阶段的新股列表。
	Fetch(ctx context.Context, scope Scope) ([]Issue, error)
}

// Manager 按顺序尝试多个来源，返回第一个成功的结果。
type Manager struct {
	providers []Provider
}

// NewManager 创建一个按给定顺序降级的数据源管理器。
func NewManager(providers ...Provider) *Manager {
	return &Manager{providers: providers}
}

// ErrNoProvider 表示没有任何可用数据源。
var ErrNoProvider = errors.New("没有可用的新股数据源")

// Fetch 依次尝试各来源，第一个成功的立即返回。全部失败时返回聚合错误。
func (m *Manager) Fetch(ctx context.Context, scope Scope) (Result, error) {
	if !scope.Valid() {
		return Result{}, fmt.Errorf("未知的发行阶段 %q", scope)
	}
	if len(m.providers) == 0 {
		return Result{}, ErrNoProvider
	}
	var errs []error
	for i, p := range m.providers {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		issues, err := p.Fetch(ctx, scope)
		if err == nil {
			res := Result{
				Issues:      issues,
				Source:      p.ID(),
				SourceLabel: p.Label(),
			}
			if i > 0 {
				res.Fallback = true
				res.Warning = fmt.Sprintf("主数据源暂时不可用，已切换到备用数据源（%s）。数据可能略有延迟，建议稍后刷新确认。", p.Label())
			}
			return res, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p.Label(), err))
	}
	return Result{}, fmt.Errorf("所有新股数据源均不可用：%w", errors.Join(errs...))
}
