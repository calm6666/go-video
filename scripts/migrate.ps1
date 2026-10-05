# 执行 deploy/migrations/<service> 下的 MySQL 迁移。
# 默认连接参数取自 services/<service>/etc/*.yaml 的 DataSource（与服务运行时同源，避免两处配置漂移）。
# 已应用的迁移记录在每个库的 schema_migrations 表：按文件名排序，只执行一次。
param(
    [ValidateSet('status', 'up')]
    [string]$Action = 'status',
    [string]$Service = '',
    # 覆盖 yaml DataSource 的可选参数，用于 CI / 共享实例；默认留空表示按服务读取。
    [string]$OverrideHost = '',
    [int]$OverridePort = 0,
    [string]$OverrideUser = '',
    [string]$OverridePassword = $null
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$migrationsRoot = Join-Path $repoRoot 'deploy\migrations'

function Get-MysqlClient {
    $cmd = Get-Command mysql -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $found = Get-ChildItem 'C:\Program Files\MySQL' -Filter 'mysql.exe' -Recurse -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($found) { return $found.FullName }
    throw 'mysql client not found in PATH or C:\Program Files\MySQL; install the MySQL client first.'
}

# 解析 go-zero 风格 DSN：user:pass@tcp(host:port)/dbname?params
function Get-DataSource([string]$service) {
    $etcDir = Join-Path $repoRoot "services\$service\etc"
    if (-not (Test-Path $etcDir)) { return $null }
    $match = Select-String -Path (Join-Path $etcDir '*.yaml') -Pattern '^\s*DataSource:\s*"?([^"\s]+)"?' |
        Select-Object -First 1
    if (-not $match) { return $null }
    $parsed = [regex]::Match($match.Matches[0].Groups[1].Value,
        '^(?<user>[^:@]*)(?::(?<pass>[^@]*))?@tcp\((?<host>[^:]+):(?<port>\d+)\)/(?<db>[^?]+)')
    if (-not $parsed.Success) { return $null }
    return [pscustomobject]@{
        User = $parsed.Groups['user'].Value
        Pass = $parsed.Groups['pass'].Value
        Host = $parsed.Groups['host'].Value
        Port = $parsed.Groups['port'].Value
        Db   = $parsed.Groups['db'].Value
    }
}

function Invoke-Mysql {
    param(
        [string]$Client, $Dsn, [string]$Query
    )
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'   # 探测 schema_migrations 时允许“表不存在”
    $out = & $Client "--protocol=tcp" "--host=$($Dsn.Host)" "--port=$($Dsn.Port)" "--user=$($Dsn.User)" `
        "--password=$($Dsn.Pass)" "--default-character-set=utf8mb4" -N -e $Query 2>$null
    $code = $LASTEXITCODE
    $ErrorActionPreference = $prev
    if ($null -eq $out) { return @{ ExitCode = $code; Lines = @() } }
    return @{ ExitCode = $code; Lines = @($out) }
}

function Invoke-MigrationFile {
    param([string]$Client, $Dsn, [string]$Path)
    $argLine = "--protocol=tcp --host=$($Dsn.Host) --port=$($Dsn.Port) --user=$($Dsn.User) " +
        "--password=$($Dsn.Pass) --default-character-set=utf8mb4 $($Dsn.Db)"
    # 经 cmd 重定向按原始字节喂给 mysql 客户端：PowerShell 管道会按控制台代码页转码，破坏 UTF-8 中文注释。
    & $env:ComSpec /c "`"$Client`" $argLine < `"$Path`""
    return $LASTEXITCODE
}

function Invoke-Service([string]$client, [string]$service, $dsn) {
    $files = @(Get-ChildItem (Join-Path $migrationsRoot $service) -Filter '*.sql' | Sort-Object Name)
    if ($files.Count -eq 0) { Write-Warning "$service : no .sql files under deploy/migrations/$service"; return }

    if ($Action -eq 'up') {
        $bootstrap = "CREATE DATABASE IF NOT EXISTS ``$($dsn.Db)`` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci; " +
            "CREATE TABLE IF NOT EXISTS ``$($dsn.Db)``.schema_migrations (" +
            "version VARCHAR(255) NOT NULL PRIMARY KEY, applied_at BIGINT NOT NULL) " +
            "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='已应用迁移版本';"
        if ((Invoke-Mysql $client $dsn $bootstrap).ExitCode -ne 0) {
            throw "$service : failed to create database $($dsn.Db) or schema_migrations table"
        }
    }

    $probe = Invoke-Mysql $client $dsn "SELECT version FROM ``$($dsn.Db)``.schema_migrations;"
    $applied = @($probe.Lines | ForEach-Object { "$_".Trim() } | Where-Object { $_ })

    foreach ($file in $files) {
        if ($Action -eq 'status') {
            $state = if ($applied -contains $file.Name) { 'applied' } else { 'pending' }
            Write-Host ("{0,-28} {1,-8} {2}" -f $dsn.Db, $state, $file.Name)
            continue
        }
        if ($applied -contains $file.Name) { Write-Host "$($dsn.Db): skip $($file.Name)"; continue }
        Write-Host "$($dsn.Db): apply $($file.Name)"
        if ((Invoke-MigrationFile $client $dsn $file.FullName) -ne 0) {
            throw "$($dsn.Db): $($file.Name) failed; 修复 SQL 后重跑（迁移文件必须保持 CREATE TABLE IF NOT EXISTS 幂等）"
        }
        $now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
        $record = "INSERT INTO ``$($dsn.Db)``.schema_migrations (version, applied_at) " +
            "VALUES ('$($file.Name)', $now) ON DUPLICATE KEY UPDATE applied_at = VALUES(applied_at);"
        if ((Invoke-Mysql $client $dsn $record).ExitCode -ne 0) {
            throw "$($dsn.Db): failed to record $($file.Name) in schema_migrations"
        }
    }
}

$client = Get-MysqlClient
$targets = if ($Service) { @($Service) } else { @(Get-ChildItem $migrationsRoot -Directory | Select-Object -ExpandProperty Name) }
foreach ($service in $targets) {
    $dir = Join-Path $migrationsRoot $service
    if (-not (Test-Path $dir)) { throw "migration directory not found: $dir" }
    $dsn = Get-DataSource $service
    if (-not $dsn) { throw "$service : DataSource not found (or unsupported format) in services/$service/etc/*.yaml" }
    if ($OverrideHost) { $dsn.Host = $OverrideHost }
    if ($OverridePort) { $dsn.Port = $OverridePort }
    if ($OverrideUser) { $dsn.User = $OverrideUser }
    if ($null -ne $OverridePassword) { $dsn.Pass = $OverridePassword }
    Invoke-Service $client $service $dsn
}
Write-Host "migration $Action completed"
