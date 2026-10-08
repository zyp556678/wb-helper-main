// 凭据校验（切片 18）。
//
// 在**两个不可逆动作**之前证明凭据的真实状态：
//
//	覆盖（令牌刷新后写回）  新凭据必须 (a) 属于同一账号，(b) 真的能取到账号数据
//	删除（从池中移除凭据）  要删的这个必须确实已经不能用
//
// 这两处出错的代价不对称：**误删 / 误覆盖一份还能用的凭据，用户是没有办法恢复的**
// （要重新登录），而多留一个失效文件只是碍眼。所以所有「无法判定」的分支
// 一律倒向保守那一侧。
package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/upstream"
)

// CredentialVerifyTimeout 是只读校验的超时。
//
// 取 8 秒：它夹在「刷新成功」与「写回磁盘」之间，太长会把账号锁占住，
// 太短会在网络抖动时把有效凭据判成「无法判定」—— 而那种情况的处置是保守保留，
// 结果只是多一次无用的探测，不会做错事。
const CredentialVerifyTimeout = 8 * time.Second

// credentialVerdict 是只读校验的结论。
type credentialVerdict int

const (
	// verdictUsable 凭据可用（上游正常返回了账号数据）。
	verdictUsable credentialVerdict = iota
	// verdictRejected 上游**明确拒绝**（401，或正文明确说登录失效）：凭据确实不可用。
	//
	// 403 刻意**不算**在内，理由见 verifyCredential。
	verdictRejected
	// verdictUnknown 无法判定（超时 / 网络 / 5xx）。
	// **处置上必须与 rejected 区分开**：不能据此销毁凭据。
	verdictUnknown
)

// verifyCredential 用只读接口给出凭据状态结论。
func (p *Pool) verifyCredential(ctx context.Context, view *upstream.CredentialView, prof *upstream.Profile) (credentialVerdict, error) {
	status, err := p.client.ProbeCredential(ctx, view, prof)
	switch {
	case err == nil:
		return verdictUsable, nil
	case status == 401:
		return verdictRejected, nil
	default:
		// 403 落到这里（verdictUnknown），**不销毁凭据**。
		//
		// 上游 workbuddy-gateway 在 d5f7858 里做过同样的收紧，理由是：
		// 「普通 403 可能只是权限不足或风控拒绝，不能据此确认 Token 失效」。
		// 我们这边更该谨慎 —— rejected 的处置是销毁凭据，把一个其实还能用的号
		// 因为一次风控 403 就删掉，代价远大于多留一个坏号。
		//
		// 只收紧"只读凭据校验"这一条路径，不改变其它调用方的 403 停用规则。
		// （上游还会看正文里有没有"明确登录失效"的字样；我们这里只拿得到状态码，
		// 所以只认 401。）
		return verdictUnknown, err
	}
}

// validateRefreshedCredential 在把新凭据写回磁盘之前做两道校验。
//
//  1. **身份一致性**：新旧令牌声明的 sub（账号标识）与 realm（站点）必须一致。
//     防的是「上游串号 / 响应错配」—— 那会把 A 的新令牌写进 B 的文件，
//     而 B 从此彻底失效，且现象是「B 账号突然 401」，看不出跟刷新有关。
//  2. **可用性**：新令牌要能取到账号数据。防的是「接口 200 但实际不可用」的凭据
//     覆盖掉还能用的旧凭据。
//
// 校验接口本身不可用时**按通过处理**：刷新接口已经成功了，
// 不该因为一次探测抖动就把有效的新凭据丢掉（那会让我们停在旧令牌上直到它过期）。
func (p *Pool) validateRefreshedCredential(oldToken string, newView *upstream.CredentialView, prof *upstream.Profile) error {
	oldID, oldRealm := auth.IdentityOf(oldToken)
	newID, newRealm := auth.IdentityOf(newView.AccessToken)

	if oldID != "" && newID != "" && oldID != newID {
		return fmt.Errorf("新凭据账号标识与旧凭据不一致（%s → %s），拒绝覆盖："+
			"这种不一致通常意味着上游串号或响应错配，覆盖会让原账号失效",
			auth.ShortID(oldID), auth.ShortID(newID))
	}
	if oldRealm != "" && newRealm != "" && oldRealm != newRealm {
		return fmt.Errorf("新凭据站点与旧凭据不一致（%s → %s），拒绝覆盖", oldRealm, newRealm)
	}

	ctx, cancel := context.WithTimeout(context.Background(), CredentialVerifyTimeout)
	defer cancel()
	verdict, err := p.verifyCredential(ctx, newView, prof)
	switch verdict {
	case verdictRejected:
		return errors.New("新凭据取不到账号数据（上游明确拒绝），拒绝覆盖旧凭据")
	case verdictUnknown:
		// 探测本身不可用 → 按通过处理，但要留痕。
		p.logf("[凭据校验] 只读校验无法完成（%v），按通过处理：刷新接口已成功，不因探测抖动丢弃新凭据", err)
	}
	return nil
}

// CredentialStillUsableForDelete 判断一份即将被删除的凭据是否其实还能用。
//
// 返回 keep=true 表示**应保留**。三个分支的取舍：
//
//	无法判定 → 保留（删除不可逆，宁可留一个失效文件）
//	仍可用   → 保留（这正是要防的「把还能用的凭据销毁掉」）
//	明确不可用 → 可以删
//
// prof 作为参数显式传入，而不是内部调 `acc.Profile()`：这样这条隐私敏感的
// 判断就可以对着假上游做完整单测（三个分支都能验），而不必真的打线上接口。
func (p *Pool) CredentialStillUsableForDelete(ctx context.Context, acc *Account, prof *upstream.Profile) (keep bool, note string) {
	if acc == nil {
		return false, "账号为空，按已失效处理"
	}
	view := acc.View()
	if view == nil || view.AccessToken == "" {
		return false, "无访问令牌可校验，按已失效处理"
	}
	verdict, err := p.verifyCredential(ctx, view, prof)
	switch verdict {
	case verdictUsable:
		return true, "只读校验显示凭据仍可取到账号数据，已保留凭据文件"
	case verdictUnknown:
		return true, fmt.Sprintf("只读校验无法完成（%v），已保守保留凭据文件", err)
	default:
		return false, "只读校验确认凭据已不可用"
	}
}
