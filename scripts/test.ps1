$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$taskTmp = Join-Path $repoRoot ".gotmp"
$cacheDir = Join-Path $taskTmp "gocache"
$goTmpDir = Join-Path $taskTmp "gotmp"
New-Item -ItemType Directory -Force -Path $cacheDir, $goTmpDir | Out-Null
$env:GOCACHE = $cacheDir
$env:GOTMPDIR = $goTmpDir

go test ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
