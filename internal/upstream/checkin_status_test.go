package upstream

import "testing"

// 上游对同一个布尔给了两种字段名，且值可能是字符串 —— 都要认。
func TestBoolFieldTolerance(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want bool
	}{
		{"snake_case bool", map[string]any{"today_checked_in": true}, true},
		{"camelCase bool", map[string]any{"todayCheckedIn": true}, true},
		{"snake_case false", map[string]any{"today_checked_in": false}, false},
		{"字符串 true", map[string]any{"today_checked_in": "true"}, true},
		{"字符串 TRUE（忽略大小写）", map[string]any{"todayCheckedIn": "TRUE"}, true},
		{"字符串 false", map[string]any{"today_checked_in": "false"}, false},
		{"两种都在时以第一个为准", map[string]any{"today_checked_in": true, "todayCheckedIn": false}, true},
		{"字段缺失", map[string]any{"other": 1}, false},
		{"nil map", nil, false},
		{"类型不认识（数字）", map[string]any{"today_checked_in": 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := boolField(c.data, "today_checked_in", "todayCheckedIn"); got != c.want {
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}
