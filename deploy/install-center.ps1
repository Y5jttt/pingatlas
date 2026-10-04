<#
.SYNOPSIS
  PingAtlas 中心端 · 一键安装 / 升级（Windows）

.DESCRIPTION
  下载官方发布二进制 → 校验 SHA-256 → 建库（可选）→ 生成 center.json → 注册计划任务（开机自启、崩溃自动重启）。
  重复执行 = 升级：只替换二进制，不会覆盖已有的 center.json。
  不依赖任何第三方服务包装工具（不需要 WinSW / NSSM / sc 服务协议）。

.EXAMPLE
  # 最简单：一条命令（管理员 PowerShell）
  irm https://cdn.jsdelivr.net/gh/Y5jttt/pingatlas@main/deploy/install-center.ps1 | iex

.EXAMPLE
  # 指定 postgres 管理员口令，让脚本自动建库建用户
  .\install-center.ps1 -PgSuperPassword 'postgres的口令'

.EXAMPLE
  # 已有数据库/连接串：直接给 DSN，跳过建库
  .\install-center.ps1 -DbDsn 'postgres://pingatlas:口令@127.0.0.1:5432/pingatlas'

.EXAMPLE
  # 离线安装（先自行下载 exe 再本地装）
  .\install-center.ps1 -Binary C:\download\pingatlas-center-windows-amd64.exe

.EXAMPLE
  # 卸载（停止并删除计划任务；-Purge 连目录一起删）
  .\install-center.ps1 -Uninstall
#>
[CmdletBinding()]
param(
  [string]$Version = 'latest',
  [string]$Dir = 'C:\pingatlas',
  [int]$Port = 18991,
  [string]$DbDsn = '',
  [string]$PgSuperUser = 'postgres',
  [string]$PgSuperPassword = '',
  [string]$DbName = 'pingatlas',
  [string]$DbUser = 'pingatlas',
  [string]$DbPass = '',
  [string]$AdminPwd = '',
  [string]$BaseUrl = '',
  [string]$Binary = '',
  [switch]$NoService,
  [switch]$Uninstall,
  [switch]$Purge,
  [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$Slug   = 'Y5jttt/pingatlas'
$Task   = 'PingAtlasCenter'
$Conf   = Join-Path $Dir 'center.json'
$Exe    = Join-Path $Dir 'pingatlas-center.exe'

function Info($m) { Write-Host "==> $m" -ForegroundColor Cyan }
function Warn($m) { Write-Host "[!] $m" -ForegroundColor Yellow }
function Die($m)  { Write-Host "[x] $m" -ForegroundColor Red; exit 1 }
function Step($m) { if ($DryRun) { Write-Host "    [dry-run] $m" } else { Info $m } }

function New-RandomHex([int]$bytes = 12) {
  $b = New-Object byte[] $bytes
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
  ($b | ForEach-Object { $_.ToString('x2') }) -join ''
}

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-Arch {
  switch ($env:PROCESSOR_ARCHITECTURE.ToUpper()) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Die "不支持的架构: $env:PROCESSOR_ARCHITECTURE" }
  }
}

function Find-Psql {
  $c = Get-Command psql.exe -ErrorAction SilentlyContinue
  if ($c) { return $c.Source }
  $cand = Get-ChildItem 'C:\Program Files\PostgreSQL\*\bin\psql.exe' -ErrorAction SilentlyContinue |
          Sort-Object FullName -Descending | Select-Object -First 1
  if ($cand) { return $cand.FullName }
  return $null
}

# ---------------- 卸载 ----------------
if ($Uninstall) {
  Info "卸载 PingAtlas 中心端"
  if (Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue) {
    Step "停止并删除计划任务 $Task"
    if (-not $DryRun) {
      Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
      Unregister-ScheduledTask -TaskName $Task -Confirm:$false
    }
  } else { Warn "没有找到计划任务 $Task" }
  if ($Purge) {
    if ($Dir -match '^[A-Za-z]:\\?$' -or $Dir.Length -lt 4) { Die "拒绝删除 $Dir（看起来是盘根目录），请显式指定一个安装子目录" }
    $procs = Get-Process pingatlas-center -ErrorAction SilentlyContinue
    if ($procs) { Step "结束进程"; if (-not $DryRun) { $procs | Stop-Process -Force } }
    Step "删除目录 $Dir"; if (-not $DryRun) { Remove-Item -Recurse -Force $Dir }
  } else {
    Warn "保留了 $Dir（含 center.json 与数据库口令）。要一起删就加 -Purge"
  }
  Info "卸载完成"
  return
}

# ---------------- 前置检查 ----------------
if (-not $DryRun -and -not (Test-Admin)) {
  Die "请用【管理员身份】的 PowerShell 运行（要注册计划任务、写 $Dir）"
}
$arch  = Get-Arch
$asset = "pingatlas-center-windows-$arch.exe"
Info "目标环境: Windows/$arch，安装目录 $Dir，端口 $Port"

# 版本解析
if ($Version -eq 'latest') {
  try {
    $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$Slug/releases/latest" -TimeoutSec 30
    $Version = $rel.tag_name
  } catch {
    Die "取最新版本失败（网络问题）。可显式指定 -Version v0.3.1；或用 -Binary 本地安装"
  }
}
$base = if ($BaseUrl) { "$($BaseUrl.TrimEnd('/'))/$Version" } else { "https://github.com/$Slug/releases/download/$Version" }
Info "将安装版本: $Version（$asset）"

# ---------------- 获取二进制 ----------------
if (-not (Test-Path $Dir)) { Step "创建目录 $Dir"; if (-not $DryRun) { New-Item -ItemType Directory -Force -Path $Dir | Out-Null } }
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("pa-" + (New-RandomHex 6))
if (-not $DryRun) { New-Item -ItemType Directory -Force -Path $tmp | Out-Null }

if ($Binary) {
  if (-not (Test-Path $Binary)) { Die "指定的本地文件不存在: $Binary" }
  Step "使用本地文件 $Binary（跳过下载与校验）"
  Warn "使用本地文件安装，已跳过 SHA-256 校验（请自行确认文件来源可信）"
  if (-not $DryRun) { Copy-Item $Binary $Exe -Force }
} else {
  Step "下载 $asset（来源 $base）"
  $dl = Join-Path $tmp $asset
  if ($DryRun) {
    Info "dry-run：跳过实际下载与校验"
  } else {
  try {
    Invoke-WebRequest -Uri "$base/$asset" -OutFile $dl -TimeoutSec 300 -UseBasicParsing
  } catch {
    Die @"
下载失败。常见原因：所在网络访问 github.com 的 release 下载受限。
  可选做法：
   1) 换镜像：  .\install-center.ps1 -BaseUrl https://<你的镜像>/$Slug
   2) 本地安装：在能联网的机器上下载 $asset，拷到本机后执行
                .\install-center.ps1 -Binary C:\路径\$asset
"@
  }
  Step "下载 checksums.txt 并校验 SHA-256"
  $sums = Join-Path $tmp 'checksums.txt'
  try { Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -TimeoutSec 120 -UseBasicParsing }
  catch { Die "取不到 checksums.txt，无法校验完整性；请用 -BaseUrl 指定能同时提供它的镜像，或用 -Binary 本地安装" }
  if (-not $DryRun) {
    $want = (Get-Content $sums | Where-Object { $_ -match ([regex]::Escape($asset) + '\s*$') } | Select-Object -First 1)
    if (-not $want) { Die "checksums.txt 里没有 $asset" }
    $wantHash = ($want -split '\s+')[0].ToLower()
    $gotHash  = (Get-FileHash $dl -Algorithm SHA256).Hash.ToLower()
    if ($wantHash -ne $gotHash) { Die "SHA-256 校验失败，已中止（期望 $wantHash，实际 $gotHash）" }
    Info "SHA-256 校验通过"
    Copy-Item $dl $Exe -Force
  }
  }
}
if (-not $DryRun) { Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }

# ---------------- 数据库 ----------------
$psql = Find-Psql
if ($DbDsn) {
  Info "使用你提供的连接串（跳过建库）"
} else {
  if (-not $psql) { Die "没找到 psql.exe。请先安装 PostgreSQL 16 + TimescaleDB，或用 -DbDsn 指定已有数据库" }
  if (-not $DryRun -and -not $PgSuperPassword) {
    Die "自动建库需要 postgres 超级用户口令：请加 -PgSuperPassword '口令'；或改用 -DbDsn 'postgres://用户:口令@127.0.0.1:5432/库名'"
  }
  if (-not $DbPass) { $DbPass = New-RandomHex }
  $env:PGPASSWORD = $PgSuperPassword
  Info "检查 PostgreSQL / TimescaleDB 并准备数据库"
  $ext = & $psql -U $PgSuperUser -h 127.0.0.1 -tAc "select 1 from pg_available_extensions where name='timescaledb'"
  if ($LASTEXITCODE -ne 0) { Die "连接 PostgreSQL 失败（用户 $PgSuperUser / 口令是否正确？）" }
  if ($ext.Trim() -ne '1') { Die "PostgreSQL 里没有 timescaledb 扩展。请先装 TimescaleDB 并在 postgresql.conf 的 shared_preload_libraries 里加载，再重跑" }
  $hasRole = & $psql -U $PgSuperUser -h 127.0.0.1 -tAc "select 1 from pg_roles where rolname='$DbUser'"
  if ($hasRole.Trim() -ne '1') {
    Step "创建数据库用户 $DbUser"
    if (-not $DryRun) { & $psql -U $PgSuperUser -h 127.0.0.1 -q -c "create role ""$DbUser"" login password '$DbPass'" | Out-Null }
  } else {
    Step "更新 $DbUser 的口令"
    if (-not $DryRun) { & $psql -U $PgSuperUser -h 127.0.0.1 -q -c "alter role ""$DbUser"" with password '$DbPass'" | Out-Null }
  }
  $hasDb = & $psql -U $PgSuperUser -h 127.0.0.1 -tAc "select 1 from pg_database where datname='$DbName'"
  if ($hasDb.Trim() -ne '1') {
    Step "创建数据库 $DbName"
    if (-not $DryRun) { & $psql -U $PgSuperUser -h 127.0.0.1 -q -c "create database ""$DbName"" owner ""$DbUser""" | Out-Null }
  }
  Step "启用 TimescaleDB 扩展（表结构由中心首次启动时自动创建）"
  if (-not $DryRun) { & $psql -U $PgSuperUser -h 127.0.0.1 -q -d $DbName -c 'create extension if not exists timescaledb' | Out-Null }
  Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
  $DbDsn = "postgres://${DbUser}:${DbPass}@127.0.0.1:5432/$DbName"
}

# ---------------- 配置 ----------------
if (Test-Path $Conf) {
  Info "已存在 $Conf，保持不变（升级不会覆盖你的配置）"
} else {
  if (-not $AdminPwd) { $AdminPwd = New-RandomHex }
  Step "生成 $Conf（数据库口令与管理密码随机、ACL 收紧到当前用户）"
  if (-not $DryRun) {
    $json = [ordered]@{ DB = $DbDsn; AdminPwd = $AdminPwd; Token = '' } | ConvertTo-Json
    [IO.File]::WriteAllText($Conf, $json, (New-Object Text.UTF8Encoding($false)))
    # 服务以 SYSTEM 运行，必须同时授权 SYSTEM 读取；只给当前用户会导致服务读不到配置
    icacls $Conf /inheritance:r /grant:r "SYSTEM:(R)" /grant:r "$($env:USERNAME):(R,W)" | Out-Null
  }
}

# ---------------- 计划任务（开机自启 + 崩溃重启） ----------------
if (-not $NoService) {
  Step "注册计划任务 $Task（开机自启、失败自动重启、以 SYSTEM 运行）"
  if (-not $DryRun) {
    $action  = New-ScheduledTaskAction -Execute $Exe -Argument '--conf center.json' -WorkingDirectory $Dir
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $set     = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
                 -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
    $princ   = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    Register-ScheduledTask -TaskName $Task -Action $action -Trigger $trigger -Settings $set `
      -Principal $princ -Force | Out-Null
    Start-ScheduledTask -TaskName $Task
    Start-Sleep -Seconds 3
    $st = (Get-ScheduledTask -TaskName $Task).State
    if ($st -eq 'Running') { Info "中心已启动（计划任务 $Task）" }
    else { Warn "任务状态为 $st，请看日志：$Dir\ 下的输出或事件查看器（任务计划程序 → $Task）" }
  }
}

# ---------------- 收尾 ----------------
$showPwd = if (Test-Path $Conf) { (Get-Content $Conf -Raw | ConvertFrom-Json).AdminPwd } else { $AdminPwd }
Write-Host ""
Write-Host "────────────────────────────────────────────────────────"
Write-Host " PingAtlas 中心安装完成 ($Version)"
Write-Host ""
Write-Host "  面板地址 : http://127.0.0.1:$Port/admin/"
Write-Host "  管理密码 : $showPwd"
Write-Host ""
Write-Host "  中心只监听 127.0.0.1（安全默认）。从外部访问请二选一："
Write-Host "    · 反向代理（推荐）：nginx/Caddy/IIS ARR 把 https://你的域名 反代到 127.0.0.1:$Port"
Write-Host "    · 临时隧道：ssh -L $Port:127.0.0.1:$Port 用户名@这台机器"
Write-Host ""
Write-Host "  加节点：登录面板 → 添加节点 → 复制一次性安装码 → 在那台机器上按提示执行"
Write-Host "  查看状态：Get-ScheduledTask $Task | Select State"
Write-Host "  卸载：.\install-center.ps1 -Uninstall [-Purge]"
Write-Host "────────────────────────────────────────────────────────"
