package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wowh/ipo-book/internal/ledger"
	"github.com/wowh/ipo-book/internal/source"
)

// endpointDoc 是一条接口的自述，供 agent 先看清单再动手。
type endpointDoc struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// apiError 是失败响应。code 用于程序判断，error 是给人看的中文说明。
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// 中文说明不要被转义成 \uXXXX，日志与终端里要能直接读。
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: message, Code: code})
}

// fail 按错误类型挑状态码。
func fail(w http.ResponseWriter, err error) {
	status, code := statusFor(err)
	writeError(w, status, code, err.Error())
}

// statusFor 把领域错误映射成 HTTP 状态码。
//
// 领域层返回的是给用户看的中文句子，这里按错误值判断类型；实在认不出来的，
// 再用措辞兜一层，保证 agent 能区分「填错了」和「程序出问题」。
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, ledger.ErrDuplicateEntry):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ledger.ErrMissingBroker),
		errors.Is(err, ledger.ErrMissingCode),
		errors.Is(err, ledger.ErrMissingAccount),
		errors.Is(err, ledger.ErrNegativeValue),
		errors.Is(err, ledger.ErrSoldOverAllotted),
		errors.Is(err, ledger.ErrNoAllotment),
		errors.Is(err, ledger.ErrBadDate),
		errors.Is(err, ledger.ErrSaleShares),
		errors.Is(err, ledger.ErrSalePrice),
		errors.Is(err, ledger.ErrTooManySales),
		errors.Is(err, ledger.ErrRateRange):
		return http.StatusBadRequest, "invalid"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "不存在"):
		return http.StatusNotFound, "not_found"
	case strings.Contains(message, "已经存在"), strings.Contains(message, "已有记录"):
		return http.StatusConflict, "conflict"
	case strings.Contains(message, "不能"), strings.Contains(message, "请填写"),
		strings.Contains(message, "请选择"), strings.Contains(message, "内容为空"),
		strings.Contains(message, "不是有效"):
		return http.StatusBadRequest, "invalid"
	}
	return http.StatusInternalServerError, "internal"
}

// decodeMap 把请求体解析成「字段名 → 原始值」，便于按提交的字段局部更新。
func decodeMap(r *http.Request) (map[string]json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, errors.New("读取请求体失败")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, errors.New("请求体需要是 JSON 对象，字段名见 GET /api")
	}
	return fields, nil
}

// parseID 读取路径里的数字 ID。
func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "路径里的 id 需要是正整数")
		return 0, false
	}
	return id, true
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	docs := append([]endpointDoc(nil), s.docs...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"app":       s.appName,
		"version":   s.version,
		"baseUrl":   s.BaseURL(),
		"discovery": s.DiscoveryPath(),
		"pointer":   func() string { p, _ := PointerPath(); return p }(),
		"skill": map[string]string{
			"dir":     s.SkillDir(),
			"read":    "GET /api/skill",
			"export":  "POST /api/skill/export",
			"content": "SKILL.md 是给 agent 的使用说明，references/api.md 是字段速查",
		},
		"docs": docs,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.svc.Snapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"app":             s.appName,
		"version":         s.version,
		"entries":         len(snapshot.Entries),
		"brokers":         len(snapshot.Brokers),
		"issues":          len(snapshot.Issues),
		"issuesUpdatedAt": snapshot.Meta.IssuesUpdatedAt,
		"dataPath":        snapshot.Meta.DataPath,
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.svc.Snapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.svc.Snapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot.Summary)
}

// handleStats 返回按年份、月份、券商账户筛选后的收益统计。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := ledger.StatsFilter{}
	if text := strings.TrimSpace(query.Get("brokerId")); text != "" {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "brokerId 需要是整数")
			return
		}
		filter.BrokerID = value
	}
	for _, item := range []struct {
		key   string
		field *int
	}{{"year", &filter.Year}, {"month", &filter.Month}} {
		text := strings.TrimSpace(query.Get(item.key))
		if text == "" {
			continue
		}
		value, err := strconv.Atoi(text)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", item.key+" 需要是整数")
			return
		}
		*item.field = value
	}
	if err := filter.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	stats, err := s.svc.Stats(r.Context(), filter)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	scope := source.Scope(strings.TrimSpace(query.Get("scope")))
	if scope == "" {
		scope = source.ScopeAll
	}
	if !scope.Valid() {
		writeError(w, http.StatusBadRequest, "invalid", "scope 只能是 all、issuing 或 done")
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(query.Get("q")))
	limit := 0
	if text := query.Get("limit"); text != "" {
		value, err := strconv.Atoi(text)
		if err != nil || value < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "limit 需要是非负整数")
			return
		}
		limit = value
	}

	snapshot, err := s.svc.Snapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	today := time.Now().Format(time.DateOnly)
	items := make([]source.Issue, 0, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		// 与界面同一口径：还没上市或上市日未到都算发行中。
		listed := issue.ListingDate != "" && issue.ListingDate < today
		if scope == source.ScopeIssuing && listed {
			continue
		}
		if scope == source.ScopeDone && !listed {
			continue
		}
		if keyword != "" && !matchIssue(issue, keyword) {
			continue
		}
		items = append(items, issue)
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(items),
		"scope":  string(scope),
		"issues": items,
	})
}

// matchIssue 判断新股是否命中关键词（名称、申购代码、证券代码）。
func matchIssue(issue source.Issue, keyword string) bool {
	return strings.Contains(strings.ToLower(issue.Name), keyword) ||
		strings.Contains(issue.FxCode, keyword) ||
		strings.Contains(issue.StockCode, keyword)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	scope := string(source.ScopeAll)
	if raw, ok := fields["scope"]; ok {
		value, err := asString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "scope 需要是文本")
			return
		}
		scope = value
	}
	result, err := s.svc.Refresh(r.Context(), scope)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleBrokers(w http.ResponseWriter, r *http.Request) {
	brokers, err := s.svc.Brokers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(brokers), "brokers": brokers})
}

func (s *Server) handleSaveBroker(w http.ResponseWriter, r *http.Request) {
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	var broker ledger.Broker
	if err := applyBroker(fields, &broker); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	saved, err := s.svc.SaveBroker(r.Context(), broker)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handlePatchBroker(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	brokers, err := s.svc.Brokers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	var target ledger.Broker
	found := false
	for _, broker := range brokers {
		if broker.ID == id {
			target, found = broker, true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("券商账户 %d 不存在", id))
		return
	}
	if err := applyBroker(fields, &target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	saved, err := s.svc.SaveBroker(r.Context(), target)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleDeleteBroker(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := s.svc.DeleteBroker(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	entries, err := s.svc.Entries(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	fxCode := strings.TrimSpace(query.Get("fxCode"))
	status := strings.TrimSpace(query.Get("status"))
	brokerID, hasBroker := int64(0), false
	if text := query.Get("brokerId"); text != "" {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "brokerId 需要是整数")
			return
		}
		brokerID, hasBroker = value, true
	}

	items := make([]ledger.View, 0, len(entries))
	for _, entry := range entries {
		if fxCode != "" && entry.FxCode != fxCode {
			continue
		}
		if hasBroker && entry.BrokerID != brokerID {
			continue
		}
		if status != "" && entry.Status != status {
			continue
		}
		items = append(items, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "entries": items})
}

func (s *Server) handleEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	entry, err := s.svc.Entry(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleSaveEntry(w http.ResponseWriter, r *http.Request) {
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	var entry ledger.Entry
	if err := applyEntry(fields, &entry); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	saved, err := s.svc.SaveEntry(r.Context(), entry)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handlePatchEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	current, err := s.svc.Entry(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	entry := current.Entry
	if err := applyEntry(fields, &entry); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	entry.ID = id
	saved, err := s.svc.SaveEntry(r.Context(), entry)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := s.svc.DeleteEntry(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// parseSaleID 读取路径里的卖出明细 ID。
func parseSaleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("saleId"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "路径里的 saleId 需要是正整数")
		return 0, false
	}
	return id, true
}

// handleAppendSale 给一条记录追加一笔卖出。
func (s *Server) handleAppendSale(w http.ResponseWriter, r *http.Request) {
	entryID, ok := parseID(w, r)
	if !ok {
		return
	}
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	sale, err := decodeSale(fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	view, err := s.svc.AppendSale(r.Context(), entryID, sale)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// handlePatchSale 整笔修改某一笔卖出。
//
// 语义是「替换这一笔」，没有局部更新：一笔成交只有股数、价格、金额、费用几个
// 数字，只改其中一个而其余留空的组合没有意义。
func (s *Server) handlePatchSale(w http.ResponseWriter, r *http.Request) {
	entryID, ok := parseID(w, r)
	if !ok {
		return
	}
	saleID, ok := parseSaleID(w, r)
	if !ok {
		return
	}
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	sale, err := decodeSale(fields)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	view, err := s.svc.UpdateSale(r.Context(), entryID, saleID, sale)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleDeleteSale 删除某一笔卖出。
func (s *Server) handleDeleteSale(w http.ResponseWriter, r *http.Request) {
	entryID, ok := parseID(w, r)
	if !ok {
		return
	}
	saleID, ok := parseSaleID(w, r)
	if !ok {
		return
	}
	view, err := s.svc.RemoveSale(r.Context(), entryID, saleID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	payload, err := s.svc.ExportJSON(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	// 备份本身就是 JSON 文本，原样返回，agent 可以直接存成文件。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, payload)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	// 恢复会整体替换账本，必须显式确认，避免 agent 顺手调用就把数据冲掉。
	if r.URL.Query().Get("confirm") != "replace" {
		writeError(w, http.StatusBadRequest, "confirm_required",
			"恢复会替换当前账本。确认要恢复时请在查询参数里加 confirm=replace")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", "读取请求体失败")
		return
	}
	imported, err := s.svc.ImportJSON(r.Context(), string(body))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported})
}

func (s *Server) handleSkillInfo(w http.ResponseWriter, r *http.Request) {
	dir := s.SkillDir()
	files, err := listSkillFiles(dir)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dir":      dir,
		"exported": len(files) > 0,
		"files":    files,
	})
}

func (s *Server) handleSkillExport(w http.ResponseWriter, r *http.Request) {
	fields, err := decodeMap(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	dir := s.SkillDir()
	if raw, ok := fields["dir"]; ok {
		value, err := asString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "dir 需要是文本")
			return
		}
		if value != "" {
			dir = value
		}
	}
	if err := s.ExportSkill(dir); err != nil {
		fail(w, err)
		return
	}
	files, err := listSkillFiles(dir)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dir": dir, "files": files})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found",
		"没有这个接口。用 GET /api 查看接口清单，skill 包里的 references/api.md 有完整说明")
}

// listSkillFiles 列出 skill 目录里的文件（相对路径）。
func listSkillFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}
