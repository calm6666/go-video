$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$taskTmp = Join-Path $repoRoot ".gotmp"
$cacheDir = Join-Path $taskTmp "gocache"
$goTmpDir = Join-Path $taskTmp "gotmp"
New-Item -ItemType Directory -Force -Path $cacheDir, $goTmpDir | Out-Null
$env:GOCACHE = $cacheDir
$env:GOTMPDIR = $goTmpDir

# -p 1：整树并发链接会撞 Windows 页面文件上限（errno=1455），与 docs/commands.md §6 一致。
go test -p 1 ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
