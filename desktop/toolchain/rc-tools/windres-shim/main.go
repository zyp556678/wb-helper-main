// windres 转发器：为 windres 注入 --preprocessor 参数。
//
// 背景：windres 需要 C 预处理器，而 rustup 的 windows-gnu 工具链里没有可用的
// 预处理器（gcc 缺 cc1，见 desktop/toolchain/rc-tools/rcpp）。tauri-winres 只会
// 从 PATH 找 windres，且其 set_windres_path 是空实现，无法从外部指定参数。
//
// 因此这里用转发器顶替 windres 这个名字，在转发时插入
//
//     --preprocessor=<同目录的 rcpp.exe>
//
// 真品 windres 放在子目录 windres-bin/ 里，与它需要的运行库 DLL 同目录。
//
// 注意：与 dlltool 一样，GNU 工具会用**自身文件名**推导配套工具名，所以真品必须
// 保留 `windres.exe` 这个名字 —— 故采用「外层顶替 + 子目录保真名」的结构。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "windres-shim: 无法定位自身路径:", err)
		os.Exit(1)
	}
	dir := filepath.Dir(self)
	real := filepath.Join(dir, "windres-bin", "windres.exe")
	rcpp := filepath.Join(dir, "rcpp.exe")

	if _, err := os.Stat(real); err != nil {
		fmt.Fprintf(os.Stderr, "windres-shim: 找不到真正的 windres（期望在 %s）\n", real)
		os.Exit(1)
	}
	if _, err := os.Stat(rcpp); err != nil {
		fmt.Fprintf(os.Stderr, "windres-shim: 找不到预处理器 rcpp（期望在 %s）\n", rcpp)
		os.Exit(1)
	}

	args := os.Args[1:]

	// 若调用方已自带预处理器设置，则不再插入，避免参数重复。
	explicit := false
	for _, a := range args {
		if strings.HasPrefix(a, "--preprocessor") {
			explicit = true
			break
		}
	}

	forward := make([]string, 0, len(args)+1)
	if !explicit {
		forward = append(forward, "--preprocessor="+rcpp)
	}
	forward = append(forward, args...)

	cmd := exec.Command(real, forward...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "windres-shim: 调用 windres 失败:", err)
		os.Exit(1)
	}
}
