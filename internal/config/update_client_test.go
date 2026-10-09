package config

import (
	"net/http"
	"testing"
)

// 「检查更新」用的客户端：要么复用显式配置的 -proxy，要么回落到环境变量代理，
// 且**绝不**改动对话流量那份 Transport 的出站策略。
// 更新检查用的客户端要认环境变量代理，但**不能**因此改动对话流量的出站策略。
func TestUpdateClientFallsBackToEnvProxy(t *testing.T) {
	cfg := &Config{}
	if err := cfg.buildClients(); err != nil {
		t.Fatalf("装配客户端失败: %v", err)
	}
	// 没配 -proxy 时，Control 不带代理，UpdateClient 应当带上环境变量代理。
	control, _ := cfg.Control.Transport.(*http.Transport)
	if control == nil || control.Proxy != nil {
		t.Fatalf("未配 -proxy 时 Control 不该设代理，实际 %+v", control)
	}
	updateTr, ok := cfg.UpdateClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("UpdateClient 应当是一个带 *http.Transport 的客户端")
	}
	if updateTr.Proxy == nil {
		t.Error("UpdateClient 应当回落到环境变量代理")
	}
	// 关键：Clone 出来的 Transport 与 Control 的那份不能是同一个对象，
	// 否则改 Proxy 会连带改掉对话流量的出站策略。
	if updateTr == control {
		t.Error("UpdateClient 不该复用 Control 的 Transport 实例")
	}
	if control.Proxy != nil {
		t.Error("Control 的代理设置被改动了")
	}
}

// 显式配了 -proxy 就与对话流量同一出口（同一条 Transport）。
func TestUpdateClientPrefersExplicitProxy(t *testing.T) {
	cfg := &Config{ProxyURL: "http://127.0.0.1:7897"}
	if err := cfg.buildClients(); err != nil {
		t.Fatalf("装配客户端失败: %v", err)
	}
	if cfg.UpdateClient() != cfg.Control {
		t.Error("配了 -proxy 时应当直接复用 Control")
	}
}

// -proxy 非法时 buildClients 应当报错（既有行为），顺带钉住 UpdateClient 的前置条件。
func TestUpdateClientRequiresBuiltClients(t *testing.T) {
	cfg := &Config{ProxyURL: "://bad"}
	if err := cfg.buildClients(); err == nil {
		t.Fatal("非法代理地址应当报错")
	}
}
