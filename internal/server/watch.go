package server

import (
	"context"
	"os"
	"time"

	"workbuddy-gateway/internal/config"
)

// 本文件实现 config.json 的**文件监听热加载**（对照上游 wb-gateway v1.14.0 的
// runtimeConfigLoop + runtimeConfigWatcher）。
//
// 为什么需要它：面板保存配置一直是热生效的，但**手动编辑 config.json 不会生效** ——
// 运行层只在启动时读一次。于是「SSH 上去改一行、重启网关」成了唯一的办法，
// 而在途会话会被这次重启全部掐断。文件监听补上这一半：改完保存，1 秒内生效。
//
// 三条刻意的取舍：
//  1. **按内容摘要轮询，不用 inotify**：配置文件只有几 KB，1 秒一次读+算摘要可以忽略；
//     inotify 反而要自己处理编辑器「写临时文件再改名」、容器里挂载事件丢失、队列溢出。
//  2. **坏配置保留上一份有效配置**，不中断服务：文件是用户手写的，语法/取值错误很正常，
//     为此让整个网关停摆或退回默认值都是过度反应。同一份坏内容只告警一次。
//  3. **不改监听端口**：端口来自命令行（启动层），文件里也没有这个字段 ——
//     不需要「端口变更需重启」这类特例。
const configWatchInterval = time.Second

// watchConfigLoop 周期检查 config.json 是否被外部改动，改了就把新配置热加载进内存。
func (s *Server) watchConfigLoop(ctx context.Context) {
	ticker := time.NewTicker(configWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reloadConfigIfChanged()
		}
	}
}

// reloadConfigIfChanged 读一次 config.json：内容变了就重建配置并热生效。
func (s *Server) reloadConfigIfChanged() {
	path := s.config().ConfigFile
	data, err := os.ReadFile(path)
	if err != nil {
		// 文件被删掉 / 暂时读不到：保留上一份有效配置。删除配置文件不是「回到默认值」的
		// 语义（那会让运行中的网关在一次误删后丢掉全部治理参数），下次 tick 再看。
		return
	}
	if !s.cfgWatch.Changed(data) {
		return
	}

	next, err := s.config().ReloadFromFile()
	if err != nil {
		if s.cfgWatch.MarkFailed(data) {
			s.logf("[配置] config.json 的改动被拒绝，保留上一份有效配置: %v", err)
		}
		return
	}

	old := s.config()
	s.cfgPtr.Store(next)
	s.cfgWatch.MarkApplied(data)
	s.applyRuntimeConfig()

	// 装配期字段如实告知：文件改到了它们，本轮不会生效（用户不必猜「为什么没变」）。
	if fields := restartRequiredFields(old, next); len(fields) > 0 {
		s.logf("[配置] 检测到 config.json 改动，已热加载；以下字段需重启进程才生效: %v", fields)
		return
	}
	s.logf("[配置] 检测到 config.json 改动，已热加载")
}

// restartRequiredFields 报告两份配置之间「改了但本轮不会生效」的字段。
//
// 现在只剩 `server.read_timeout`：它约束的是 http.Server 这个启动时构造一次的组件。
// 调试开关（debug.enabled / debug.full_requests）已改为热生效 ——
// sink 开合由 applyRuntimeConfig 里的 debuglog.Apply 负责，重复调用是幂等的。
func restartRequiredFields(oldC, newC *config.Config) []string {
	var out []string
	if oldC == nil || newC == nil {
		return out
	}
	if oldC.Server.ReadTimeout != newC.Server.ReadTimeout {
		out = append(out, "server.read_timeout")
	}
	return out
}
