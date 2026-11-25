# 注册/更新 ClashNetWatch 计划任务 —— 目标：只要机器有网，进程就必须活着。
#
# 五个触发点，任何一个命中都会拉起进程（已有实例在跑时进程会立刻自己退出，无副作用）：
#   1. 登录后 15 秒
#   2. 网络配置文件「已连接」事件（NetworkProfile 10000）—— 连上网就一定活
#   3. 从睡眠唤醒（Power-Troubleshooter 1）
#   4. 每分钟心跳 —— 兜底"进程被强杀/挂死且没有新事件"的情况（实测空启动 23ms，可忽略）
#
# 注：不加「开机时」触发器——它以 SYSTEM 身份运行，注册需要管理员权限，而本任务要在
# 用户会话里写 HKCU，必须用 Interactive 身份。开机后登录触发即可覆盖。
#
# 实测：外部强杀进程时，任务计划的"失败后重启"不会触发（被记成 0xFFFFFFFF 的异常结束），
# 真正把进程拉回来的是心跳；因此心跳间隔取 1 分钟，最坏中断 ≤ 1 分钟。
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

$dir  = Split-Path -Parent $MyInvocation.MyCommand.Path
$exe  = Join-Path $dir 'netwatch.exe'
$name = 'ClashNetWatch'
if (-not (Test-Path $exe)) { throw "找不到 $exe，请先编译" }

$action = New-ScheduledTaskAction -Execute $exe -WorkingDirectory $dir

$logon = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$logon.Delay = 'PT15S'

function New-EventTrigger([string]$path, [string]$query) {
    $cim = Get-CimClass -ClassName MSFT_TaskEventTrigger -Namespace Root/Microsoft/Windows/TaskScheduler
    $t = New-CimInstance -CimClass $cim -ClientOnly
    $t.Enabled = $true
    $t.Subscription = "<QueryList><Query Id=`"0`" Path=`"$path`"><Select Path=`"$path`">$query</Select></Query></QueryList>"
    return $t
}

# 网络一连上就拉起（"必须活着"的主触发器）
$netUp = New-EventTrigger 'Microsoft-Windows-NetworkProfile/Operational' `
    "*[System[(EventID=10000)]]"

# 从睡眠恢复
$resume = New-EventTrigger 'System' `
    "*[System[Provider[@Name='Microsoft-Windows-Power-Troubleshooter'] and EventID=1]]"

# 心跳：每分钟尝试启动一次，运行中会被单实例互斥体挡掉（实测 23ms/次）
$heartbeat = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) `
    -RepetitionInterval (New-TimeSpan -Minutes 1)

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -StartWhenAvailable -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartInterval (New-TimeSpan -Minutes 1) -RestartCount 999

$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited

Register-ScheduledTask -TaskName $name -Action $action `
    -Trigger @($logon, $netUp, $resume, $heartbeat) `
    -Settings $settings -Principal $principal -Force `
    -Description 'Clash Verge 网络感知：连到目标热点自动开系统代理+TUN，其它网络自动关；常驻保活' | Out-Null

$t = Get-ScheduledTask -TaskName $name
if ($t.Triggers.Count -ne 4) { throw "注册后触发器数量异常：$($t.Triggers.Count)（期望 4）" }
"ClashNetWatch registered | State={0} | Triggers={1}" -f $t.State, $t.Triggers.Count
foreach ($tr in $t.Triggers) { "  - {0} enabled={1}" -f $tr.CimClass.CimClassName, $tr.Enabled }
