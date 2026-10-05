// dlltool 参数适配器（备用方案）。
//
// 背景：rustc 在 x86_64-pc-windows-gnu 目标上调用 dlltool 生成 raw-dylib 的
// import library 时，固定传入
//
//     --temp-prefix kernel32.dll:
//
// 注意结尾的**冒号**。在 Linux/macOS 上 `kernel32.dll:t.o` 是合法文件名，
// 但在 Windows 上 `:` 是路径分隔/ADS 语法，dlltool 会报
//
//     error reading kernel32.dll:t.o: No such file or directory
//
// 且产物为 0 字节，导致 windows-sys / parking_lot_core / getrandom 编译失败。
//
// 本程序把 `--temp-prefix` 的值里的冒号替换为下划线，并追加进程号，
// 使临时文件名既合法又不会在 cargo 并行编译时互相覆盖；其余参数原样透传。
//
// 用法：与 dlltool-real.exe 放在同一目录，自身命名为 dlltool.exe。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dlltool-shim: 无法定位自身路径:", err)
		os.Exit(1)
	}
	// 真品必须保留 `dlltool.exe` 这个名字，并与 as.exe 同目录。
	//
	// GNU binutils 的工具会用**自身文件名**推导配套工具的名字，进而从自己所在目录
	// 查找它们。实测把真品改名为 dlltool-real.exe 后，它会因找不到 as 而报
	// "CreateProcess"（同一个二进制、同一个参数，仅改名字就失败）。
	//
	// 所以真品放在子目录 dlltool-bin/ 里、保持原名 dlltool.exe，配套 as.exe 与
	// 运行库 DLL 也在那里；本适配器留在外层顶替名字，仅转发。
	real := filepath.Join(filepath.Dir(self), "dlltool-bin", "dlltool.exe")
	if _, err := os.Stat(real); err != nil {
		fmt.Fprintf(os.Stderr, "dlltool-shim: 找不到真正的 dlltool（期望在 %s）\n", real)
		os.Exit(1)
	}

	args := os.Args[1:]
	rewritten := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		cur := args[i]
		// 参数可能是 `--temp-prefix=value` 形式，也可能分开两段，都要处理
		if strings.HasPrefix(cur, "--temp-prefix=") {
			value := strings.TrimPrefix(cur, "--temp-prefix=")
			rewritten = append(rewritten, "--temp-prefix="+sanitize(value))
			continue
		}
		rewritten = append(rewritten, cur)
		if cur == "--temp-prefix" && i+1 < len(args) {
			rewritten = append(rewritten, sanitize(args[i+1]))
			i++
		}
	}

	cmd := exec.Command(real, rewritten...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "dlltool-shim: 调用 dlltool-real 失败:", err)
		os.Exit(1)
	}
}

// 把前缀里的非法字符换掉，并加进程号保证并行编译时不撞名。
func sanitize(prefix string) string {
	replaced := strings.NewReplacer(":", "_", "\\", "_", "/", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_").Replace(prefix)
	return replaced + strconv.Itoa(os.Getpid())
}
