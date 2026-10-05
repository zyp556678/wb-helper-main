<#
  安装/卸载前清理占用安装目录的进程，并等待文件锁真正释放。

  ## 为什么需要这个脚本

  Tauri 安装包自带的 CheckIfAppIsRunning 只会结束 workbuddy-gateway-desktop.exe，
  但真正占着待覆盖文件的 `bin\workbuddy-gateway.exe` 与 `bin\wb-local-agent.exe`
  是**它拉起的子进程**。用 taskkill 强杀父进程时，父进程的退出回调不会执行，
  子进程就成了孤儿继续持有文件句柄 —— 用户看到的现象是
  「安装程序提示先退出软件、点了确定，仍然报文件被占用」。

  另外，用户也可能自己用命令行启动了网关（那就不存在父子关系），同样会占住文件。

  ## 为什么按路径而不是按映像名杀

  用户完全可能同时跑着仓库里的开发实例（同名、不同目录）。按映像名杀会误伤
  与我们无关的进程；只处理 `$InstallDir` 下的才是职责范围内的事。

  ## 退出码

  0 = 文件已可写（或本来就不存在）；1 = 仍有文件被占用。
  钩子据此决定是继续安装还是给出可读的提示后中止 —— 比让 NSIS 在复制文件时
  抛出「无法打开要写入的文件」要好排查得多。
#>
param(
  [Parameter(Mandatory = $true)]
  [string]$InstallDir
)

$ErrorActionPreference = 'SilentlyContinue'

# 目录前缀统一成「以反斜杠结尾」，避免 D:\App 误匹配 D:\App2 下的进程。
$prefix = $InstallDir.TrimEnd('\') + '\'

function Get-InstalledProcesses {
  param([string]$Prefix)
  Get-CimInstance Win32_Process |
    Where-Object {
      $_.ExecutablePath -and
      $_.ExecutablePath.StartsWith($Prefix, [System.StringComparison]::OrdinalIgnoreCase)
    }
}

function Get-LockedFiles {
  param([string[]]$Paths)
  $locked = @()
  foreach ($path in $Paths) {
    if (-not (Test-Path -LiteralPath $path)) { continue }
    try {
      # Share=None：只要有**任何**其它句柄（哪怕只读）都算被占用。
      # 这正是复制文件所需的独占写权限，用它当判据比看进程列表可靠。
      $stream = [System.IO.File]::Open(
        $path,
        [System.IO.FileMode]::Open,
        [System.IO.FileAccess]::ReadWrite,
        [System.IO.FileShare]::None)
      $stream.Close()
      $stream.Dispose()
    } catch {
      $locked += $path
    }
  }
  return $locked
}

# ---- 1. 结束安装目录下的进程 ----
#
# 分轮次进行：先结束壳与网关，再结束本机代理。顺序不能反 ——
# 网关的守护逻辑发现本机代理消失会把它重新拉起，先杀代理等于白杀。
for ($round = 0; $round -lt 6; $round++) {
  $procs = @(Get-InstalledProcesses -Prefix $prefix)
  if ($procs.Count -eq 0) { break }

  $ordered = $procs | Sort-Object -Property @{
    Expression = {
      switch -Wildcard ($_.Name) {
        'workbuddy-gateway-desktop.exe' { 0 }
        'workbuddy-gateway.exe'         { 1 }
        default                         { 2 }
      }
    }
  }

  foreach ($proc in $ordered) {
    Write-Output ("停止 {0} (PID {1})" -f $proc.Name, $proc.ProcessId)
    Stop-Process -Id $proc.ProcessId -Force -ErrorAction SilentlyContinue
  }
  Start-Sleep -Milliseconds 400
}

# ---- 2. 等文件锁释放 ----
#
# 进程被杀不等于句柄立刻回收（内核要等清理完成）。这里最多等 10 秒，
# 而不是"杀完就往下走" —— 那正是原来那个偶发失败的来源。
$files = @(
  (Join-Path $InstallDir 'workbuddy-gateway-desktop.exe'),
  (Join-Path $InstallDir 'bin\workbuddy-gateway.exe'),
  (Join-Path $InstallDir 'bin\wb-local-agent.exe'),
  (Join-Path $InstallDir 'WebView2Loader.dll')
)

for ($i = 0; $i -lt 40; $i++) {
  $locked = @(Get-LockedFiles -Paths $files)
  if ($locked.Count -eq 0) {
    Write-Output '目标文件均已可写，安装继续'
    exit 0
  }
  Start-Sleep -Milliseconds 250
}

$locked = @(Get-LockedFiles -Paths $files)
if ($locked.Count -gt 0) {
  Write-Output ('以下文件仍被占用: ' + ($locked -join '; '))
  exit 1
}

Write-Output '目标文件均已可写，安装继续'
exit 0
