# 构建 TeleAgent 云电脑助手 GUI（Windows 可双击 exe）。
# 说明：本地 go 版本低于 go.mod 声明时会自动复制到临时目录并把 go 行降级，
# 从而不修改仓库中的 go.mod。
param(
    [string]$OutputDir = "dist",
    [string]$Icon = "internal/winui/assets/ctyunhelper.ico",
    [string]$Manifest = "internal/winui/assets/ctyunhelper.exe.manifest"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
    $mainGo = Join-Path $repo "cmd/teleagent-gui/main_windows.go"
    if (-not (Test-Path $mainGo)) { throw "找不到 $mainGo" }
    $versionMatch = Select-String -Path $mainGo -Pattern 'appVersion\s*=\s*"([^"]+)"' | Select-Object -First 1
    if (-not $versionMatch) { throw "无法从 main_windows.go 读取 appVersion" }
    $version = $versionMatch.Matches[0].Groups[1].Value

    $go = Get-Command go -ErrorAction SilentlyContinue
    if (-not $go) { throw "找不到 go，可执行文件未安装或不在 PATH" }

    $buildDir = $repo
    $goModLine = (Select-String -Path (Join-Path $repo "go.mod") -Pattern '^go\s+([0-9.]+)$').Matches[0].Groups[1].Value
    $localVersion = (& go version) -replace '^go version go([0-9.]+).*$', '$1'
    $localParts = $localVersion.Split('.') | ForEach-Object { [int]$_ }
    $modParts = $goModLine.Split('.') | ForEach-Object { [int]$_ }
    $localSupports = ($localParts[0] -gt $modParts[0]) -or ($localParts[0] -eq $modParts[0] -and $localParts[1] -ge $modParts[1])
    if (-not $localSupports) {
        $stamp = "teleagent-gui-build"
        $buildDir = Join-Path $env:TEMP $stamp
        if (Test-Path $buildDir) { Remove-Item $buildDir -Recurse -Force }
        New-Item -ItemType Directory -Path $buildDir | Out-Null
        robocopy $repo $buildDir /E /XD dist .git /NFL /NDL /NJH /NJS /NP | Out-Null
        $goModPath = Join-Path $buildDir "go.mod"
        (Get-Content $goModPath -Raw) -replace '(?m)^go\s+[0-9.]+$', "go $localVersion" | Set-Content $goModPath -NoNewline
        Write-Host "本地 go $localVersion 低于 go.mod 的 $goModLine，改用临时目录构建：$buildDir"
    }

    $outDir = Join-Path $repo $OutputDir
    if (-not (Test-Path $outDir)) { New-Item -ItemType Directory -Path $outDir | Out-Null }
    $exeName = "TeleAgentHelper-v$version.exe"
    $exePath = Join-Path $outDir $exeName

    Push-Location $buildDir
    try {
        & go build -trimpath -ldflags "-H=windowsgui -s -w" -o $exePath ./cmd/teleagent-gui
        if ($LASTEXITCODE -ne 0) { throw "go build 失败（退出码 $LASTEXITCODE）" }
    } finally { Pop-Location }

    $embed = Join-Path $repo "scripts/embed-icon-windows.ps1"
    if ((Test-Path $embed) -and (Test-Path (Join-Path $repo $Icon))) {
        & $embed -Executable $exePath -Icon (Join-Path $repo $Icon) -Manifest (Join-Path $repo $Manifest)
        if ($LASTEXITCODE -ne 0) { throw "嵌入图标失败（退出码 $LASTEXITCODE）" }
    } else {
        Write-Warning "未找到 embed-icon-windows.ps1 或图标文件，跳过图标嵌入。"
    }

    $size = (Get-Item $exePath).Length
    Write-Host "构建完成：$exePath（$([math]::Round($size / 1MB, 2)) MB）"
} finally {
    Pop-Location
}
