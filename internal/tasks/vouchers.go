package tasks

// 开学季券码查询（只读）。
//
// 抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等）只能从上游的券码端点读出来，
// 而它**不在奖励流水里**（流水只记积分）。用户真正要做的事是「把券码复制给店员核销」，
// 所以这里按账号分组返回，并保留上游给的券码本体与有效期。

import (
	"context"
	"fmt"
	"strings"
)

// VoucherGroup 是一个账号的券码集合。
type VoucherGroup struct {
	Account   string        `json:"account"`
	UID       string        `json:"uid"`
	Nickname  string        `json:"nickname"`
	SiteLabel string        `json:"site_label"`
	Vouchers  []VoucherItem `json:"vouchers"`
	Note      string        `json:"note,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// VoucherItem 是面板展示用的券码条目（字段名与上游对齐）。
type VoucherItem struct {
	GrantID   int64  `json:"grant_id"`
	PrizeName string `json:"prize_name,omitempty"`
	SKUCode   string `json:"sku_code,omitempty"`
	Code      string `json:"code"`
	ValidFrom string `json:"valid_from,omitempty"`
	ValidTo   string `json:"valid_to,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
}

// SchoolVouchers 查询券码（accountID 非空时只查那一个账号）。
//
// 国际站账号跳过并标注：开学季只有国内站有，对国际站发请求只会拿到 404，
// 而把「本就不存在的能力」渲染成红色故障会让用户去查网络。
func (m *Manager) SchoolVouchers(ctx context.Context, accountID string) ([]VoucherGroup, error) {
	var list []target
	if strings.TrimSpace(accountID) == "" {
		list = m.targets(nil)
	} else {
		list = m.targets([]string{accountID})
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("没有可用的账号")
	}
	out := make([]VoucherGroup, 0, len(list))
	for _, tg := range list {
		g := VoucherGroup{
			Account: tg.ID, UID: tg.UID, Nickname: tg.Nick,
			SiteLabel: tg.SiteName, Vouchers: []VoucherItem{},
		}
		if !tg.Prof.SupportsGrowthActivity() {
			g.Note = tg.SiteName + "没有开学季活动"
			out = append(out, g)
			continue
		}
		items, err := m.client.SchoolVouchers(ctx, tg.Cred, tg.Prof)
		if err != nil {
			g.Error = err.Error()
			out = append(out, g)
			continue
		}
		for _, it := range items {
			g.Vouchers = append(g.Vouchers, VoucherItem{
				GrantID: it.GrantID, PrizeName: it.PrizeName, SKUCode: it.SKUCode,
				Code: it.Code, ValidFrom: it.ValidFrom, ValidTo: it.ValidTo, GrantedAt: it.GrantedAt,
			})
		}
		out = append(out, g)
	}
	return out, nil
}
