package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wowh/ipo-book/internal/ledger"
)

// maxFixedWhole 是按定点数解析时允许的最大整数位数，防住离谱输入导致的溢出。
const maxFixedWhole = 18

// asText 取出 JSON 里的数字或字符串原文。
//
// 金额与费率既允许写成数字 4.48，也允许写成字符串 "4.48"：模型生成的 JSON
// 两种写法都常见，而字符串能保证小数位原样传进来，不会被浮点吃掉精度。
func asText(raw json.RawMessage) (string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return "", errors.New("取值不能为空")
	}
	if text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", errors.New("需要文本")
		}
		return strings.TrimSpace(s), nil
	}
	return text, nil
}

// asBool 读取布尔字段。true、"true"、1、"1" 都算真，其余算假。
func asBool(raw json.RawMessage) bool {
	text, err := asText(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// asString 读取文本字段。
func asString(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errors.New("需要文本")
	}
	return strings.TrimSpace(s), nil
}

// asFixed 把十进制数字按 10^digits 放大成整数，全程按字符处理。
//
// 不经过 float64：传 "4.485" 时要拿到确定的 449，而不是 4.48 的浮点近似被
// 截断成 448。多出的小数位四舍五入，只在这里舍入一次。
func asFixed(raw json.RawMessage, digits int) (int64, error) {
	text, err := asText(raw)
	if err != nil {
		return 0, err
	}
	original := text
	negative := false
	if text != "" && (text[0] == '+' || text[0] == '-') {
		negative = text[0] == '-'
		text = text[1:]
	}
	whole, frac, hasDot := strings.Cut(text, ".")
	if hasDot && strings.Contains(frac, ".") {
		return 0, fmt.Errorf("%q 不是合法数字", original)
	}
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("%q 不是合法数字", original)
	}
	if len(whole) > maxFixedWhole-digits {
		return 0, fmt.Errorf("%q 太大", original)
	}

	var value int64
	for _, ch := range whole {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("%q 不是合法数字", original)
		}
		value = value*10 + int64(ch-'0')
	}
	scale := int64(1)
	for i := 0; i < digits; i++ {
		scale *= 10
	}
	value *= scale

	var tail int64
	for i := 0; i < len(frac) && i < digits; i++ {
		ch := frac[i]
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("%q 不是合法数字", original)
		}
		tail = tail*10 + int64(ch-'0')
	}
	for i := len(frac); i < digits; i++ {
		tail *= 10
	}
	// 多出的第一位决定进位，其余位不影响。
	if len(frac) > digits && frac[digits] >= '5' {
		tail++
	}
	value += tail
	if negative {
		value = -value
	}
	return value, nil
}

// asInt 读取整数字段。股数、条数这类量不允许带小数。
func asInt(raw json.RawMessage) (int64, error) {
	text, err := asText(raw)
	if err != nil {
		return 0, err
	}
	if strings.Contains(text, ".") {
		// "1000.00" 这类写法可以接受，只要小数部分全是 0。
		_, frac, _ := strings.Cut(text, ".")
		if strings.Trim(frac, "0") != "" {
			return 0, fmt.Errorf("%q 需要整数", text)
		}
	}
	return asFixed(raw, 0)
}

// asYuan 把「元」换成「分」。
func asYuan(raw json.RawMessage) (int64, error) { return asFixed(raw, 2) }

// 记录里可以用「元」提交的字段，值指向分字段。
var entryMoney = map[string]func(*ledger.Entry) *int64{
	"issuePrice":  func(e *ledger.Entry) *int64 { return &e.IssuePriceFen },
	"allotPrice":  func(e *ledger.Entry) *int64 { return &e.AllotPriceFen },
	"capitalCost": func(e *ledger.Entry) *int64 { return &e.CapitalCostFen },
	"otherFee":    func(e *ledger.Entry) *int64 { return &e.OtherFeeFen },
}

// 记录里以「股」为单位的字段。
var entryShares = map[string]func(*ledger.Entry) *int64{
	"applyShares":    func(e *ledger.Entry) *int64 { return &e.ApplyShares },
	"allottedShares": func(e *ledger.Entry) *int64 { return &e.AllottedShares },
}

// 记录里的日期字段。
var entryDates = map[string]func(*ledger.Entry) *string{
	"purchaseDate": func(e *ledger.Entry) *string { return &e.PurchaseDate },
	"freezeDate":   func(e *ledger.Entry) *string { return &e.FreezeDate },
	"refundDate":   func(e *ledger.Entry) *string { return &e.RefundDate },
}

// legacySaleMoney 是早期「只记一笔卖出」写法里的金额字段。
//
// 卖出改成明细数组后这些字段仍然接受：请求里没有 sales、只给了它们时，折算成
// 一笔成交。老脚本、老的 skill 说明因此不会立刻失效，但新写的调用应该用 sales。
var legacySaleMoney = map[string]func(*ledger.Sale) *int64{
	"sellPrice":  func(s *ledger.Sale) *int64 { return &s.PriceFen },
	"sellAmount": func(s *ledger.Sale) *int64 { return &s.AmountFen },
	"sellFee":    func(s *ledger.Sale) *int64 { return &s.FeeFen },
}

// saleMoney 是一笔卖出可以用「元」提交的字段。
var saleMoney = map[string]func(*ledger.Sale) *int64{
	"price":  func(s *ledger.Sale) *int64 { return &s.PriceFen },
	"amount": func(s *ledger.Sale) *int64 { return &s.AmountFen },
	"fee":    func(s *ledger.Sale) *int64 { return &s.FeeFen },
}

// saleFields 是一笔卖出接受的字段名。
func saleFields() []string {
	names := []string{"date", "shares", "note"}
	for key := range saleMoney {
		names = append(names, key, key+"Fen")
	}
	sort.Strings(names)
	return names
}

// entryFields 是记录接口接受的字段名，用于给出可操作的报错。
func entryFields() []string {
	names := []string{
		"brokerId", "fxCode", "stockCode", "name", "note", "sales",
		// 单笔卖出的老写法，仍然接受，内部折算成一笔明细。
		"soldShares", "sellDate",
	}
	for key := range legacySaleMoney {
		names = append(names, key, key+"Fen")
	}
	for key := range entryMoney {
		names = append(names, key, key+"Fen")
	}
	for key := range entryShares {
		names = append(names, key)
	}
	for key := range entryDates {
		names = append(names, key)
	}
	sort.Strings(names)
	return names
}

// 券商账户里可以用「元」提交的字段。
var brokerMoney = map[string]func(*ledger.Broker) *int64{
	"minCommission": func(b *ledger.Broker) *int64 { return &b.MinCommissionFen },
}

var brokerRates = map[string]struct {
	setter func(*ledger.Broker) *int64
	digits int
}{
	// 万分之 2.5 = 250 ppm，两位小数正好等于以百万分之一计。
	"commissionWan": {func(b *ledger.Broker) *int64 { return &b.CommissionPPM }, 2},
	// 3.85% = 38500 ppm，四位小数。
	"annualRatePct": {func(b *ledger.Broker) *int64 { return &b.AnnualRatePPM }, 4},
}

// brokerFields 是券商账户接口接受的字段名。
func brokerFields() []string {
	names := []string{"name", "account", "note", "commissionPpm", "minCommissionFen", "annualRatePpm"}
	for key := range brokerMoney {
		names = append(names, key, key+"Fen")
	}
	for key := range brokerRates {
		names = append(names, key)
	}
	sort.Strings(names)
	return names
}

// applyEntry 把请求里的字段写进记录。
//
// 只要提交了的字段，就按提交的值覆盖，没提交的保持原样，因此同一个函数既能
// 用于新增也能用于局部修改。金额默认按「元」理解，带 Fen 后缀的按「分」。
//
// 卖出以 sales 数组为准。没有 sales 却给了 soldShares、sellPrice 这些单笔字段
// 时，把它们折算成一笔成交（早期写法）；两者同时出现直接报错，不去猜调用方的
// 本意。
func applyEntry(m map[string]json.RawMessage, e *ledger.Entry) error {
	var (
		legacy     ledger.Sale
		legacyUsed bool
		salesGiven bool
	)
	for key, raw := range m {
		if key == "sales" {
			sales, err := asSales(raw)
			if err != nil {
				return fieldError(key, err)
			}
			e.Sales = sales
			salesGiven = true
			continue
		}
		if key == "soldShares" {
			v, err := asInt(raw)
			if err != nil {
				return fieldError(key, err)
			}
			legacy.Shares, legacyUsed = v, true
			continue
		}
		if key == "sellDate" {
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			legacy.Date, legacyUsed = v, true
			continue
		}
		if setter, ok := legacySaleMoney[key]; ok {
			v, err := asYuan(raw)
			if err != nil {
				return fieldError(key, err)
			}
			*setter(&legacy) = v
			legacyUsed = true
			continue
		}
		if base, ok := strings.CutSuffix(key, "Fen"); ok {
			if setter, ok := legacySaleMoney[base]; ok {
				v, err := asInt(raw)
				if err != nil {
					return fieldError(key, err)
				}
				*setter(&legacy) = v
				legacyUsed = true
				continue
			}
			if setter, ok := entryMoney[base]; ok {
				v, err := asInt(raw)
				if err != nil {
					return fieldError(key, err)
				}
				*setter(e) = v
				continue
			}
		}
		if setter, ok := entryMoney[key]; ok {
			v, err := asYuan(raw)
			if err != nil {
				return fieldError(key, err)
			}
			*setter(e) = v
			continue
		}
		if setter, ok := entryShares[key]; ok {
			v, err := asInt(raw)
			if err != nil {
				return fieldError(key, err)
			}
			*setter(e) = v
			continue
		}
		if setter, ok := entryDates[key]; ok {
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			*setter(e) = v
			continue
		}
		switch key {
		case "brokerId":
			v, err := asInt(raw)
			if err != nil {
				return fieldError(key, err)
			}
			e.BrokerID = v
		case "fxCode":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			e.FxCode = v
		case "stockCode":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			e.StockCode = v
		case "name":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			e.Name = v
		case "note":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			e.Note = v
		case "id":
			// 路径里的 id 才是权威，请求体里的 id 忽略。
		case "account":
			return errors.New("账户请用 brokerId 指定，account 只是记录里的名称快照")
		default:
			if isComputedEntryField(key) {
				return fmt.Errorf("%s 是计算出来的只读字段，不能写入", key)
			}
			return fmt.Errorf("未知字段 %q，可用字段：%s", key, strings.Join(entryFields(), "、"))
		}
	}
	if legacyUsed {
		if salesGiven {
			return errors.New("sales 与 soldShares、sellPrice 等单笔卖出字段不能同时提交，请只用 sales")
		}
		e.Sales = nil
		switch {
		case legacy.Shares > 0:
			e.Sales = []ledger.Sale{legacy}
		case legacy.PriceFen > 0 || legacy.AmountFen > 0 || legacy.FeeFen > 0:
			// 给了价格或费用却没给股数，多半是漏了 soldShares，别静默丢掉。
			return fieldError("soldShares", ledger.ErrSaleShares)
		}
	}
	return nil
}

// decodeSale 解析一笔卖出。金额默认按「元」，带 Fen 后缀的按「分」。
func decodeSale(fields map[string]json.RawMessage) (ledger.Sale, error) {
	var s ledger.Sale
	for key, raw := range fields {
		if setter, ok := saleMoney[key]; ok {
			v, err := asYuan(raw)
			if err != nil {
				return ledger.Sale{}, fieldError(key, err)
			}
			*setter(&s) = v
			continue
		}
		if base, ok := strings.CutSuffix(key, "Fen"); ok {
			if setter, ok := saleMoney[base]; ok {
				v, err := asInt(raw)
				if err != nil {
					return ledger.Sale{}, fieldError(key, err)
				}
				*setter(&s) = v
				continue
			}
		}
		switch key {
		case "id":
			// 明细 ID 由库分配，提交值忽略。
		case "date":
			v, err := asString(raw)
			if err != nil {
				return ledger.Sale{}, fieldError(key, err)
			}
			s.Date = v
		case "shares":
			v, err := asInt(raw)
			if err != nil {
				return ledger.Sale{}, fieldError(key, err)
			}
			s.Shares = v
		case "note":
			v, err := asString(raw)
			if err != nil {
				return ledger.Sale{}, fieldError(key, err)
			}
			s.Note = v
		default:
			return ledger.Sale{}, fmt.Errorf("未知字段 %q，一笔卖出可用字段：%s",
				key, strings.Join(saleFields(), "、"))
		}
	}
	if err := s.Validate(); err != nil {
		return ledger.Sale{}, err
	}
	return s, nil
}

// asSales 解析卖出明细数组。
func asSales(raw json.RawMessage) ([]ledger.Sale, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New(`sales 需要是数组，如 [{"date":"2026-10-20","shares":500,"price":"27.38"}]`)
	}
	if len(items) > ledger.MaxSales {
		return nil, fmt.Errorf("一条记录最多 %d 笔卖出", ledger.MaxSales)
	}
	sales := make([]ledger.Sale, 0, len(items))
	for i, item := range items {
		sale, err := decodeSale(item)
		if err != nil {
			return nil, fmt.Errorf("第 %d 笔：%w", i+1, err)
		}
		sales = append(sales, sale)
	}
	return sales, nil
}

// isComputedEntryField 判断字段名是否属于派生字段。
func isComputedEntryField(key string) bool {
	switch key {
	case "status", "costFen", "totalCostFen", "realizedFen", "returnBps",
		"applyFundsFen", "freezeDays", "holdingShares", "holdingCostFen",
		"capitalCostEstimated", "feeEstimated", "freezeEstimated", "saleCount":
		return true
	}
	return false
}

// applyBroker 把请求里的字段写进券商账户。
func applyBroker(m map[string]json.RawMessage, b *ledger.Broker) error {
	for key, raw := range m {
		if setter, ok := brokerMoney[key]; ok {
			v, err := asYuan(raw)
			if err != nil {
				return fieldError(key, err)
			}
			*setter(b) = v
			continue
		}
		if base, ok := strings.CutSuffix(key, "Fen"); ok {
			if setter, ok := brokerMoney[base]; ok {
				v, err := asInt(raw)
				if err != nil {
					return fieldError(key, err)
				}
				*setter(b) = v
				continue
			}
		}
		if rule, ok := brokerRates[key]; ok {
			v, err := asFixed(raw, rule.digits)
			if err != nil {
				return fieldError(key, err)
			}
			*rule.setter(b) = v
			continue
		}
		switch key {
		case "name":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			b.Name = v
		case "account":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			b.Account = v
		case "note":
			v, err := asString(raw)
			if err != nil {
				return fieldError(key, err)
			}
			b.Note = v
		case "commissionPpm":
			v, err := asInt(raw)
			if err != nil {
				return fieldError(key, err)
			}
			b.CommissionPPM = v
		case "minCommissionFen":
			v, err := asInt(raw)
			if err != nil {
				return fieldError(key, err)
			}
			b.MinCommissionFen = v
		case "annualRatePpm":
			v, err := asInt(raw)
			if err != nil {
				return fieldError(key, err)
			}
			b.AnnualRatePPM = v
		case "id":
			// 路径里的 id 才是权威。
		default:
			return fmt.Errorf("未知字段 %q，可用字段：%s", key, strings.Join(brokerFields(), "、"))
		}
	}
	return nil
}

// fieldError 给换算失败的字段加上字段名，便于定位。
func fieldError(field string, err error) error {
	return fmt.Errorf("字段 %s：%w", field, err)
}
