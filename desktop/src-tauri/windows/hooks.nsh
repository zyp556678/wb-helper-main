; WorkBuddy Gateway —— NSIS 安装钩子
;
; ## 解决的问题
;
; 现象：应用还在后台运行时启动安装程序，安装程序提示「先退出软件」，用户点了确定，
; 安装**仍然**报「无法打开要写入的文件」。
;
; 原因有两层，缺一不可：
;
;   1. Tauri 自带的 CheckIfAppIsRunning 只结束 workbuddy-gateway-desktop.exe。
;      而真正占着待覆盖文件的 `bin\workbuddy-gateway.exe` 与 `bin\wb-local-agent.exe`
;      是壳拉起的**子进程**。taskkill 强杀父进程时，父进程的退出回调不会执行，
;      子进程成为孤儿并继续持有文件句柄。
;   2. 即便子进程会退出，NSIS 在「检查进程」之后**立刻**开始复制文件，
;      没有任何等待 —— 句柄回收需要时间，于是偶发失败。
;
; 这个钩子在文件复制**之前**（Tauri 的 PREINSTALL 位置）按路径清理进程并等锁释放，
; 两个问题一起解决。清理干净后，Tauri 内置的那次进程检查会发现没有进程在跑，
; 于是不再弹窗 —— 用户不会再被问一次。
;
; ## 为什么按路径而不是按映像名
;
; 用户可能同时跑着仓库里的开发实例（同名、不同目录）。只处理 $INSTDIR 下的进程，
; 不越界去杀与我们无关的东西。

; 钩子文件所在目录。必须在**顶层**取：宏体展开时 ${__FILEDIR__} 会变成
; 生成脚本的目录（cargo-target 下的临时目录），而不是钩子目录。
!define WB_HOOK_DIR "${__FILEDIR__}"

!macro WB_STOP_INSTALLED_PROCESSES
  InitPluginsDir
  SetOutPath $PLUGINSDIR
  File "${WB_HOOK_DIR}\stop-installed-processes.ps1"

  DetailPrint "正在关闭已安装的 WorkBuddy Gateway 进程…"
  nsExec::ExecToLog 'powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$PLUGINSDIR\stop-installed-processes.ps1" -InstallDir "$INSTDIR"'
  Pop $0

  ; 还原输出目录。留在 $PLUGINSDIR 会把主程序与资源全写进临时目录，
  ; 装完看起来"装成功了"但开始菜单指向的程序根本不存在。
  SetOutPath $INSTDIR

  ; 清理失败时给出可操作的提示再中止，而不是让 NSIS 在复制文件时抛出
  ; 「无法打开要写入的文件」—— 那句话不告诉用户是哪个进程在占用。
  ${If} $0 != 0
    MessageBox MB_ICONSTOP "无法关闭正在运行的 WorkBuddy Gateway，安装已中止。$\r$\n$\r$\n请在任务管理器中结束「WorkBuddy Gateway」相关进程后重试。$\r$\n若仍失败，请重启电脑后再安装。"
    Abort
  ${EndIf}
!macroend

!macro NSIS_HOOK_PREINSTALL
  ; 编译期自检开关，默认关闭。核对安装侧接线：
  ;   makensis -DWB_VERIFY_HOOKS installer.nsi
  ; 宏若真的展开，编译会立即中断并打印下面这行；没有中断就是没接上。
  ;
  ; 为什么需要它：`.nsi` 是 makensis 的**输入**，里面出现
  ; `!insertmacro NSIS_HOOK_PREINSTALL` 只说明模板留了这个位置，
  ; 并不证明宏展开了 —— 而展开失败是**静默**的，装完才发现没生效。
  ;
  ; 用 `!error` 而不是 `!echo`/`!warning`：实测这个 NSIS 版本里宏体内的
  ; `!echo` 不产生任何输出；`!warning` 虽可见但要翻日志找，只有 `!error` 的
  ; 「中断 / 不中断」是二值且不可能看错。消息保持 ASCII —— 控制台按 GBK
  ; 解码 UTF-8，中文会变成乱码（实测 `!warning` 里的中文就是乱码）。
  !ifdef WB_VERIFY_HOOKS
    !error "WB hook expanded: preinstall will stop installed processes"
  !endif
  !insertmacro WB_STOP_INSTALLED_PROCESSES
!macroend

; 卸载侧接线单独自检（用另一个开关）：
;   makensis -DWB_VERIFY_HOOKS_UNINSTALL installer.nsi
; 分开开关的原因：`!error` 会中断编译，一个开关没法同时验证两处。
!macro NSIS_HOOK_PREUNINSTALL
  !ifdef WB_VERIFY_HOOKS_UNINSTALL
    !error "WB hook expanded: preuninstall will stop installed processes"
  !endif
  !insertmacro WB_STOP_INSTALLED_PROCESSES
!macroend
