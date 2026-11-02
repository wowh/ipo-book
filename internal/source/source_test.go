package source

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestParseFen(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"4.48", 448},
		{"20.3", 2030},
		{"0", 0},
		{"", 0},
		{"null", 0},
		{"11.455", 1146},
		{"11.444", 1144},
		{"100", 10000},
		{" 6.10 ", 610},
	}
	for _, tc := range cases {
		if got := parseFen(tc.raw); got != tc.want {
			t.Errorf("parseFen(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestParseWhole(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"42800000", 42800000},
		{"42800000.0", 42800000},
		{"", 0},
		{"null", 0},
	}
	for _, tc := range cases {
		if got := parseWhole(tc.raw); got != tc.want {
			t.Errorf("parseWhole(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestFlexDate(t *testing.T) {
	var row struct {
		A flexDate `json:"a"`
		B flexDate `json:"b"`
		C flexDate `json:"c"`
		D flexDate `json:"d"`
	}
	payload := `{"a":{"time":1792684800000},"b":"2026-09-30","c":null,"d":"2026/05/06"}`
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, want := row.A.day(), "2026-10-23"; got != want {
		t.Errorf("Java Date = %q, want %q", got, want)
	}
	if got, want := row.B.day(), "2026-09-30"; got != want {
		t.Errorf("ISO date = %q, want %q", got, want)
	}
	if got := row.C.day(); got != "" {
		t.Errorf("null date = %q, want empty", got)
	}
	if got, want := row.D.day(), "2026-05-06"; got != want {
		t.Errorf("slash date = %q, want %q", got, want)
	}
}

func TestDecodeJSONP(t *testing.T) {
	var out []struct {
		ListInfo struct {
			TotalPages int `json:"totalPages"`
		} `json:"listInfo"`
	}
	if err := decodeJSONP([]byte(`cb([{"listInfo":{"totalPages":3}}])`), &out); err != nil {
		t.Fatalf("decodeJSONP: %v", err)
	}
	if len(out) != 1 || out[0].ListInfo.TotalPages != 3 {
		t.Fatalf("unexpected result: %+v", out)
	}
	if err := decodeJSONP([]byte(`[{"listInfo":{"totalPages":1}}]`), &out); err != nil {
		t.Fatalf("plain JSON: %v", err)
	}
}

func TestParseToken(t *testing.T) {
	body := []byte(`<script>window[_$a[0]].cookie="C3VK=0615e1; path=/; max-age=300;"</script>`)
	token, ok := parseToken(body)
	if !ok || token != "C3VK=0615e1" {
		t.Fatalf("parseToken = %q, %v", token, ok)
	}
	if _, ok := parseToken([]byte("<html>ok</html>")); ok {
		t.Fatal("parseToken should fail without a challenge")
	}
}

// TestLiveFetch 访问真实站点，验证两个数据源都能取到数据。
// 默认跳过，设置 IPO_BOOK_LIVE=1 时运行。
func TestLiveFetch(t *testing.T) {
	if os.Getenv("IPO_BOOK_LIVE") != "1" {
		t.Skip("set IPO_BOOK_LIVE=1 to hit the live BSE endpoints")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, p := range []Provider{NewBSEPrimary(), NewBSEBackup()} {
		issues, err := p.Fetch(ctx, ScopeIssuing)
		if err != nil {
			t.Fatalf("%s: %v", p.Label(), err)
		}
		t.Logf("%s 取到 %d 只发行中新股", p.Label(), len(issues))
		if len(issues) == 0 {
			t.Fatalf("%s 未取到数据", p.Label())
		}
		for i, issue := range issues {
			if i >= 3 {
				break
			}
			t.Logf("  %s %s 发行价=%d分 申购日=%s 上市日=%s", issue.FxCode, issue.Name, issue.PriceFen, issue.PurchaseDate, issue.ListingDate)
		}
	}
}
