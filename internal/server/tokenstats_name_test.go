package server

import (
	"path/filepath"
	"testing"

	"workbuddy-gateway/internal/accountmeta"
)

// 用量统计里「账号」显示什么名字。
//
// 口径必须与账号页（前端 accountDisplayName）**逐字一致**，
// 否则同一个账号在两页会叫两个名字。这里钉住四条：
// 备注优先（仅在 display_field=note 时）、否则昵称、再否则 uid、最后回退文件名。
func TestAccountDisplayName(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAccountMeta(accountmeta.New(filepath.Join(t.TempDir(), "wb-account-meta.json")))

	const id = "workbuddy-test.json" // newTestServer 造的这个账号：uid=u1，昵称=测试

	// 1. 默认：昵称。
	if got := srv.accountDisplayName(id); got != "测试" {
		t.Fatalf("默认应显示昵称「测试」，实际 %q", got)
	}

	// 2. display_field=note 且备注非空：备注优先。
	note := "工作号"
	field := accountmeta.DisplayNote
	if _, err := srv.accountMeta.Update(id, &note, &field); err != nil {
		t.Fatalf("写备注失败: %v", err)
	}
	if got := srv.accountDisplayName(id); got != note {
		t.Fatalf("display_field=note 时应显示备注 %q，实际 %q", note, got)
	}

	// 3. display_field=note 但备注为空：回退昵称（不能显示空串）。
	empty := "   "
	if _, err := srv.accountMeta.Update(id, &empty, nil); err != nil {
		t.Fatalf("清空备注失败: %v", err)
	}
	if got := srv.accountDisplayName(id); got != "测试" {
		t.Fatalf("备注为空时应回退昵称，实际 %q", got)
	}

	// 4. 有备注但 display_field 是 nickname：**不**用备注（与账号页一致）。
	field = accountmeta.DisplayNickname
	if _, err := srv.accountMeta.Update(id, &note, &field); err != nil {
		t.Fatalf("改显示字段失败: %v", err)
	}
	if got := srv.accountDisplayName(id); got != "测试" {
		t.Fatalf("display_field=nickname 时不该用备注，实际 %q", got)
	}

	// 5. 账号已不在池里（凭据被删、统计里仍有历史计数）：回退文件名而不是空白。
	if got := srv.accountDisplayName("workbuddy-已删除.json"); got != "workbuddy-已删除.json" {
		t.Fatalf("池外账号应回退文件名，实际 %q", got)
	}
}
