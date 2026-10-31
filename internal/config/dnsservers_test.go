package config

import (
	"os"
	"path/filepath"
	"testing"
)

// dns_servers 指定出站解析用的 DNS（对照 wb2api-panel 的 dns_servers）。
//
// 只接受 "ip:port" 形态：解析器要直接向该地址发 UDP 查询，缺端口就没法拨号。
// 非法项被丢弃而不是让整个配置加载失败 —— 一个写错的 DNS 服务器不该让网关起不来。
func TestDNSServersParsed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "正常列表",
			body: `{"upstream":{"dns_servers":["1.1.1.1:53","8.8.8.8:53"]}}`,
			want: []string{"1.1.1.1:53", "8.8.8.8:53"},
		},
		{
			name: "丢弃缺端口与非法项",
			body: `{"upstream":{"dns_servers":["1.1.1.1:53","bad","1.2.3.4",":53"]}}`,
			want: []string{"1.1.1.1:53"},
		},
		{
			name: "去空白",
			body: `{"upstream":{"dns_servers":["  1.1.1.1:53  "]}}`,
			want: []string{"1.1.1.1:53"},
		},
		{
			name: "留空沿用系统解析",
			body: `{"upstream":{}}`,
			want: nil,
		},
		{
			name: "全部非法时退回系统解析",
			body: `{"upstream":{"dns_servers":["bad","worse"]}}`,
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(&Config{WorkDir: dir})
			if err != nil {
				t.Fatalf("加载配置失败: %v", err)
			}
			if len(cfg.DNSServers) != len(tc.want) {
				t.Fatalf("DNSServers = %v，期望 %v", cfg.DNSServers, tc.want)
			}
			for i := range tc.want {
				if cfg.DNSServers[i] != tc.want[i] {
					t.Errorf("DNSServers[%d] = %q，期望 %q", i, cfg.DNSServers[i], tc.want[i])
				}
			}
		})
	}
}
