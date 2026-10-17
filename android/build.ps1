<#
Собирает olcvpn для Android: бэкенд (этот репозиторий), ядро olcRTC и клиент
VK TURN под android/arm64 — в app/src/main/jniLibs — и затем APK.

    .\build.ps1 -Version v0.2.0
    .\build.ps1 -SkipCores            # ядро и клиент уже собраны
    .\build.ps1 -Task installRelease  # сразу поставить на телефон по adb

Ядро и клиент собираются из тех же деревьев, что и для Windows (см. README,
«Что положить рядом» и «VK TURN»): OlConnect_manager server-v2.3.5 с
patches/olcrtc-reorder-hold.patch и vk-turn-proxy PR #183 с patches/vkturn-*.
К ним через go build -overlay добавляется по одному файлу из overlay/ — сами
деревья не меняются.

Нужны Go 1.26+, JDK 17+ и Android SDK (ANDROID_HOME или local.properties).
#>
param(
    [string]$Version = "dev",
    [string]$Core = "$PSScriptRoot\..\..\core-fix",
    [string]$VKTurn = "$PSScriptRoot\..\..\vkt-pr183",
    [switch]$SkipCores,
    [string]$Task = "assembleRelease"
)

$ErrorActionPreference = "Stop"
$jni = Join-Path $PSScriptRoot "app\src\main\jniLibs\arm64-v8a"
New-Item -ItemType Directory -Force $jni | Out-Null

# arm64 — единственная архитектура, которую Go собирает под Android без
# cgo и NDK; а телефонов на ней почти все.
$env:GOOS = "android"
$env:GOARCH = "arm64"
$env:CGO_ENABLED = "0"

function Build-Go([string]$dir, [string]$pkg, [string]$out, [string]$tags, [string]$ldflags, [hashtable]$overlay) {
    $goArgs = @("build", "-trimpath")
    if ($tags) { $goArgs += "-tags=$tags" }
    $goArgs += "-ldflags=$ldflags"
    if ($overlay) {
        $json = Join-Path $env:TEMP ("olcvpn-overlay-" + [IO.Path]::GetFileName($out) + ".json")
        $map = @{}
        foreach ($k in $overlay.Keys) { $map[(Resolve-Path $dir).Path + "\" + $k] = (Resolve-Path $overlay[$k]).Path }
        @{ Replace = $map } | ConvertTo-Json | Set-Content -Encoding ascii $json
        $goArgs += "-overlay=$json"
    }
    $goArgs += @("-o", $out, $pkg)
    Write-Host "go $($goArgs -join ' ')  (в $dir)"
    Push-Location $dir
    # Go пишет в stderr; в Windows PowerShell 5.1 при Stop это сразу ошибка.
    $ErrorActionPreference = "Continue"
    try {
        & go @goArgs 2>&1 | ForEach-Object { "$_" }
        if ($LASTEXITCODE -ne 0) { throw "сборка $out не удалась" }
    } finally {
        Pop-Location
        $ErrorActionPreference = "Stop"
    }
}

# Бэкенд: sing-box внутри него нужен с gvisor (стек TUN) и WireGuard (VK TURN).
Build-Go (Join-Path $PSScriptRoot "..") "." (Join-Path $jni "libolcvpn.so") `
    "with_gvisor,with_wireguard,badlinkname,tfogo_checklinkname0" `
    "-s -w -checklinkname=0 -X main.version=$Version" $null

if (-not $SkipCores) {
    # -checklinkname=0: pion берёт anet, а тот лезет в net.zoneCache, что
    # новые версии Go без этого флага не пропускают.
    Build-Go $Core "./cmd/olcrtc" (Join-Path $jni "libolcrtc.so") "" "-s -w -checklinkname=0" `
        @{ "cmd\olcrtc\olcvpn_android.go" = (Join-Path $PSScriptRoot "overlay\olcrtc_android.go") }
    Build-Go $VKTurn "./client" (Join-Path $jni "libvkturn.so") "" "-s -w -checklinkname=0" `
        @{ "client\olcvpn_android.go" = (Join-Path $PSScriptRoot "overlay\vkturn_android.go") }
}
foreach ($f in "libolcrtc.so", "libvkturn.so") {
    if (-not (Test-Path (Join-Path $jni $f))) { throw "нет $f — соберите без -SkipCores" }
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

$gradle = Join-Path $PSScriptRoot "gradlew.bat"
$ErrorActionPreference = "Continue"
& $gradle -p $PSScriptRoot "-Polcvpn.version=$Version" $Task 2>&1 | ForEach-Object { "$_" }
$ErrorActionPreference = "Stop"
if ($LASTEXITCODE -ne 0) { throw "gradle $Task не удался" }

if ($Task -eq "assembleRelease") {
    $apk = Join-Path $PSScriptRoot "app\build\outputs\apk\release\app-release.apk"
    $dst = Join-Path $PSScriptRoot "olcvpn-$Version-android-arm64.apk"
    Copy-Item $apk $dst -Force
    Write-Host "готово: $dst"
}
