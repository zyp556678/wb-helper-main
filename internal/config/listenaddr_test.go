package config

import "testing"

// ListenAddr 的取值优先级（对照 wb-gateway v1.14.38 的端口热切换）。
//
// config.json 的 server.addr / server.port 是**可热切换**的覆盖项：
// 配了就压过启动参数 -addr / -port，没配就沿用。这条优先级必须钉住，
// 否则「改了端口但保存后还是老地址」会变成很难查的问题。
func TestListenAddrPrecedence(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "都没有覆盖时用启动参数",
			cfg:  Config{Addr: "127.0.0.1", Port: 8317},
			want: "127.0.0.1:8317",
		},
		{
			name: "只覆盖端口",
			cfg:  Config{Addr: "127.0.0.1", Port: 8317, Server: ServerSection{Port: 9000}},
			want: "127.0.0.1:9000",
		},
		{
			name: "只覆盖地址",
			cfg:  Config{Addr: "127.0.0.1", Port: 8317, Server: ServerSection{Addr: "0.0.0.0"}},
			want: "0.0.0.0:8317",
		},
		{
			name: "两个都覆盖",
			cfg: Config{Addr: "127.0.0.1", Port: 8317,
				Server: ServerSection{Addr: "0.0.0.0", Port: 9001}},
			want: "0.0.0.0:9001",
		},
		{
			name: "地址带空白时按未配置处理",
			cfg:  Config{Addr: "127.0.0.1", Port: 8317, Server: ServerSection{Addr: "   "}},
			want: "127.0.0.1:8317",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ListenAddr(); got != tc.want {
				t.Errorf("ListenAddr() = %q，期望 %q", got, tc.want)
			}
		})
	}
}
