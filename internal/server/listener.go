package server

// ListenerSwapper 允许在进程内换监听地址（对照 wb-gateway v1.14.38）。
//
// 为什么放在这里而不是 config 层：只有 main 能持有 net.Listener 与 http.Server，
// 配置层只描述「想要哪个地址」。保存接口按下面的顺序执行，保证失败时运行状态不变：
//
//  1. 在**克隆**上合并补丁（不碰正在被热路径读的那份）；
//  2. 若监听地址变了，先 SwapListenAddr —— 新端口起得来才继续；
//  3. 起不来就直接拒绝本次保存，旧端口继续服务、内存配置也不换。
//
// 第 2 步必须早于换快照：反过来的话「配置已生效但端口没换」会让面板显示的地址
// 与实际监听不一致，而用户看到的恰恰是面板。
type ListenerSwapper interface {
	// SwapListenAddr 尝试在 addr 上建立新监听并开始接受连接。
	//
	// 失败时**必须保持原监听不变**并返回错误；成功时旧 listener 关闭
	// （只停止接受新连接，已有连接与在途流继续跑完）。
	SwapListenAddr(addr string) error
}

// SetListenerSwapper 注入监听切换实现。传 nil 表示不支持热切换。
func (s *Server) SetListenerSwapper(l ListenerSwapper) {
	s.listenerMu.Lock()
	s.listenerSwap = l
	s.listenerMu.Unlock()
}

func (s *Server) listenerSwapper() ListenerSwapper {
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()
	return s.listenerSwap
}
