package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// 上游对超过 pageSize 的请求只返回**区间内最早的一批**（total 也被截断）。
// 这个假实现如实复刻该行为：按 startTime 排序、截断、并返回同一个 total。
func fakeUsageServer(t *testing.T, all []officialUsageRow) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var starts []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
			PageSize  int    `json:"pageSize"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		mu.Lock()
		starts = append(starts, req.StartTime)
		mu.Unlock()

		var picked []officialUsageRow
		for _, row := range all {
			if row.RequestTime >= req.StartTime && row.RequestTime <= req.EndTime {
				picked = append(picked, row)
			}
		}
		// 上游按时间升序返回，并在超量时保留**最早**的一批。
		sort.Slice(picked, func(i, j int) bool { return picked[i].RequestTime < picked[j].RequestTime })
		size := req.PageSize
		if size <= 0 || size > len(picked) {
			size = len(picked)
		}
		// 上游语义：保留最早的一批，total 同时被截断。
		picked = picked[:size]

		payload := map[string]any{"code": 0, "msg": "", "data": map[string]any{
			"total": size,
			"data":  picked,
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	return srv, &starts
}

// 超过一页时必须继续扫：只取首页会拿到最老的那批，最新的（今天）永远取不到。
func TestOfficialUsageSweepsPastFirstPage(t *testing.T) {
	// 用相对今天的时间，保证都落在查询窗口内（窗口按 days 往前推）。
	all := make([]officialUsageRow, 0, 8)
	for i := 0; i < 8; i++ {
		all = append(all, officialUsageRow{
			RequestTime: time.Now().AddDate(0, 0, -i).Format("2006-01-02") + " 10:00:00",
			Model:       "m",
			Credit:      1,
			RequestID:   fmt.Sprintf("r%d", i),
		})
	}
	srv, starts := fakeUsageServer(t, all)
	defer srv.Close()

	// 把单页上限临时压到 3，模拟「一页装不下」。
	old := officialUsagePageSize
	officialUsagePageSize = 3
	defer func() { officialUsagePageSize = old }()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	sum, err := c.OfficialUsage(context.Background(), &CredentialView{UID: "u1"}, usageTestProfile(srv.URL), 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if sum.Requests != 8 {
		t.Fatalf("应扫回 8 条，实际 %d（只取首页会得到 3 条）", sum.Requests)
	}
	// 最新一条落在今天，Today 必须取到 —— 这正是「只取首页」时会丢掉的那条。
	if sum.Today != 1 {
		t.Fatalf("今日消耗应为 1（最新一天那条），实际 %v；说明滑动扫描没取到最新数据", sum.Today)
	}
	if len(*starts) < 3 {
		t.Fatalf("应至少扫描 3 轮，实际 %d 轮（起点=%v）", len(*starts), *starts)
	}
	// 去重：相邻两轮边界重叠，条数不能因此翻倍。
	if sum.Fetched != 8 {
		t.Fatalf("去重后应为 8 条，实际 %d", sum.Fetched)
	}
}

// 起点推不动时必须停止，不能死循环。
func TestOfficialUsageStopsWhenStartDoesNotAdvance(t *testing.T) {
	rows := make([]officialUsageRow, 0, 4)
	for i := 0; i < 4; i++ {
		// 全部同一时刻：推进起点拿到的还是同一批。
		rows = append(rows, officialUsageRow{
			RequestTime: time.Now().AddDate(0, 0, -1).Format("2006-01-02") + " 10:00:00",
			Model:       "m", Credit: 1,
			RequestID: fmt.Sprintf("r%d", i),
		})
	}
	srv, _ := fakeUsageServer(t, rows)
	defer srv.Close()

	old := officialUsagePageSize
	officialUsagePageSize = 2
	defer func() { officialUsagePageSize = old }()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	sum, err := c.OfficialUsage(context.Background(), &CredentialView{UID: "u1"}, usageTestProfile(srv.URL), 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 同一时间戳的 4 条只可能取到前 2 条，随后起点推不动并如实标注不完整。
	if !sum.Truncated {
		t.Fatal("起点无法推进时应标注 truncated")
	}
	if sum.Requests > 4 {
		t.Fatalf("去重失效，条数异常: %d", sum.Requests)
	}
}

// usageTestProfile 指向假上游的 Profile（Control 客户端直接打这个地址）。
func usageTestProfile(base string) *Profile {
	return &Profile{Key: "cn", Base: base, Origin: base}
}
