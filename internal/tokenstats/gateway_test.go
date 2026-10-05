package tokenstats

import "testing"

// 网关来源的账号分组：展示名放 Title，**Key 必须保持凭据文件名**（它是身份，
// 也是前端列表的 React key），且展示名与文件名相同时不写 Title ——
// 否则前端会出现上下两行一模一样。
func TestBuildGatewaySourceAccountTitles(t *testing.T) {
	src := BuildGatewaySource(GatewayInput{
		Tokens: 160,
		Accounts: []GatewayGroup{
			{Key: "workbuddy-a.json", Title: "無限進步", Requests: 2, Tokens: 100},
			{Key: "workbuddy-b.json", Title: "workbuddy-b.json", Tokens: 50},
			{Key: "workbuddy-c.json", Tokens: 10},
		},
	})

	if len(src.Sessions) != 3 {
		t.Fatalf("账号槽位应有 3 行，实际 %d", len(src.Sessions))
	}
	first := src.Sessions[0]
	if first.Key != "workbuddy-a.json" {
		t.Fatalf("Key 必须是凭据文件名，实际 %q", first.Key)
	}
	if first.Title == nil || *first.Title != "無限進步" {
		t.Fatalf("展示名应放在 Title，实际 %v", first.Title)
	}
	for _, row := range src.Sessions[1:] {
		if row.Title != nil {
			t.Fatalf("%s 的 Title 与 Key 相同或缺失，应为 nil（前端会退回 Key），实际 %q", row.Key, *row.Title)
		}
	}
}
