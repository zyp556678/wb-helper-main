package main

// `autostart` 子命令：把「开机自启」这一个系统级设置暴露给命令行。
//
// ## 为什么需要它
//
// 安装器要能把自启打开（用户勾了「开机启动」），但**不能自己重写一遍落点逻辑** ——
// 面板开关、托盘开关、安装器三条路必须操作同一条系统记录，否则会出现
// 「安装器写了一个、面板说没开」这种解释不清的状态，更糟的是两个自启项同时躺在
// 系统里抢 8317 端口。
//
// 落点的唯一定义在 internal/autostart，所以这里只做一层薄接线：把子命令转成
// 对 Enable / Disable / Query 的调用。安装器执行 `workbuddy-gateway autostart on`，
// 拿到的就是与面板完全一致的行为（含 §冲突检测，见下列说明）。
//
// ## 冲突检测会**拒绝**而不是静默执行
//
// Enable 在发现命令行版（计划任务 WorkBuddyGateway）已存在时会返回错误，
// 因为两者共用 8317 端口，同时自启必然有一个起不来，且表现为「有时能连上、
// 有时连不上」这种最难排查的故障。安装器据此只打印一句提醒，不让安装失败 ——
// 用户的安装意图是明确的，自启只是附带的偏好。

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"workbuddy-gateway/internal/autostart"
)

func runAutostart(args []string) {
	os.Exit(cmdAutostart(args, os.Stdout, os.Stderr))
}

func cmdAutostart(args []string, out, errOut *os.File) int {
	fs := flag.NewFlagSet("autostart", flag.ContinueOnError)
	fs.SetOutput(errOut)
	jsonOut := fs.Bool("json", false, "以 JSON 输出状态")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	action := "status"
	if rest := fs.Args(); len(rest) > 0 {
		action = rest[0]
	}

	var (
		st  autostart.Status
		err error
	)
	switch action {
	case "on", "enable":
		st, err = autostart.Enable()
	case "off", "disable":
		st, err = autostart.Disable()
	case "status":
		st = autostart.Query()
	default:
		fmt.Fprintf(errOut, "用法: workbuddy-gateway autostart [on|off|status] [-json]\n")
		return 2
	}

	if *jsonOut {
		out2 := struct {
			autostart.Status
			Error string `json:"error,omitempty"`
		}{Status: st}
		if err != nil {
			out2.Error = err.Error()
		}
		_ = json.NewEncoder(out).Encode(out2)
		return exitCodeFor(err)
	}

	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return exitCodeFor(err)
	}
	if !st.Supported {
		fmt.Fprintf(out, "本平台不支持自启管理%s\n", detailSuffix(st))
		return 1
	}
	if st.Enabled {
		fmt.Fprintf(out, "开机自启：已开启（%s）%s\n", st.Kind, detailSuffix(st))
	} else {
		fmt.Fprintf(out, "开机自启：未开启%s\n", detailSuffix(st))
	}
	return 0
}

func detailSuffix(st autostart.Status) string {
	if st.Detail == "" {
		return ""
	}
	return " —— " + st.Detail
}

// exitCodeFor 把自启操作的错误归一成退出码。
//
// 分 1 与 3：1 是「本平台不支持 / 一般失败」，3 专门表示「与另一形态的自启冲突」。
// 安装器据此决定是提示一句还是当成功处理 —— 冲突不该让安装失败。
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	return 1
}
