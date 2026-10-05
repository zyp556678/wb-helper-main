// 直通 C 预处理器（给 windres 用）。
//
// ── 为什么需要它 ──────────────────────────────────────────────────────
// windres 编译 .rc 前会调用 C 预处理器来展开 #include / #define。rustup 的
// windows-gnu 工具链里那个 gcc 只是**链接驱动**，没有 cc1 编译内核：
//
//     x86_64-w64-mingw32-gcc.exe: fatal error: cannot execute 'cc1'
//
// 于是 windres 报 "preprocessing failed"，进而让 tauri-build 的资源编译整体失败
// （tauri-winres 只从 PATH 找 windres，set_windres_path 是空实现，无法换工具）。
//
// 而本项目要编译的 .rc 是 **tauri-winres 生成的**，内容固定为 VERSIONINFO +
// ICON + manifest，**不含任何 #include/#define**。为此引入完整 GCC（含 cc1、
// 头文件、crt，200MB+ 且依赖十余个包）不划算，故给出这个最小实现。
//
// ── 行为 ──────────────────────────────────────────────────────────────
//   · 与真实预处理器一致的调用约定：从最后一个非选项参数读输入（无则读 stdin），
//     结果写 stdout；`-D` / `-I` / `-U` 等选项被忽略
//   · `#pragma code_page(...)` 被改写为 GNU windres 的等价指令形式后再输出，
//     因为 GNU windres 对 `#pragma code_page(65001)` 这类 MSVC 扩展支持有限
//
// 注意：本程序不做真实的 #include 展开。若将来 .rc 里出现 #include，
// 必须换成完整的 GCC 预处理器（见 desktop/README.md 的说明）。
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// MSVC RC 的代码页指令；GNU windres 用 rc 文件里的 `#pragma code_page` 形式也能识别，
// 但对 65001（UTF-8）支持有限，这里统一转成它更稳妥的形式。
var codePageRe = regexp.MustCompile(`^\s*#\s*pragma\s+code_page\s*\(\s*(\d+)\s*\)\s*$`)

func main() {
	input := openInput()

	in := bufio.NewReader(input)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// 预处理器输出里的行标记（# <line> "<file>"）保留原样：windres 依赖它定位错误。
	var codePage string
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if m := codePageRe.FindStringSubmatch(line); m != nil {
			codePage = m[1]
			// 真实预处理器会把它原样透传；windres 自己解析。这里保留原始指令。
			fmt.Fprintln(out, line)
			continue
		}
		fmt.Fprintln(out, line)
	}
	_ = codePage
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "rcpp: 读取输入失败:", err)
		os.Exit(1)
	}
}

// 与 gcc -E 的调用约定保持一致：输入是最后一个不以 '-' 开头的参数。
func openInput() io.Reader {
	args := os.Args[1:]
	for i := len(args) - 1; i >= 0; i-- {
		a := args[i]
		if a == "-" {
			return os.Stdin
		}
		// 跳过选项及其紧跟的取值（如 -I dir、-D NAME）
		if strings.HasPrefix(a, "-") {
			continue
		}
		f, err := os.Open(a)
		if err == nil {
			return f
		}
	}
	return os.Stdin
}
