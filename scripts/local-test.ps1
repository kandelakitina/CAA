#Requires -Version 7.2
param([ValidateSet('Start', 'Stop', 'Test')][string]$Action = 'Start')
$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
$local = Join-Path $repo '.local-test'
$pgBin = Join-Path $local 'postgresql/pgsql/bin'
$weed = Join-Path $local 'seaweedfs/weed.exe'
$data = Join-Path $local 'pgdata'
$settingsPath = Join-Path $local 'settings.json'

function Check-Exit([string]$step) {
    if ($LASTEXITCODE -ne 0) { throw "$step failed (exit $LASTEXITCODE)." }
}
function Wait-Port([int]$port) {
    for ($i = 0; $i -lt 60; $i++) {
        $client = [Net.Sockets.TcpClient]::new()
        try { $client.Connect('127.0.0.1', $port); return } catch { Start-Sleep -Milliseconds 500 } finally { $client.Dispose() }
    }
    throw "Local test port $port did not start. See .local-test/logs."
}
function New-Secret {
    $bytes = [byte[]]::new(32)
    [Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    return [Convert]::ToBase64String($bytes)
}
if ($Action -eq 'Stop') {
    if (Test-Path (Join-Path $data 'postmaster.pid')) {
        & "$pgBin/pg_ctl.exe" -D $data -m fast -w stop
        Check-Exit 'PostgreSQL stop'
    }
    $pidFile = Join-Path $local 'seaweed.pid'
    if (Test-Path $pidFile) {
        $process = Get-Process -Id ([int](Get-Content $pidFile)) -ErrorAction SilentlyContinue
        if ($process) {
            if ($process.Path -ne $weed.Replace('/', '\')) { throw 'PID belongs to a different executable; refusing to stop it.' }
            Stop-Process -Id $process.Id
        }
    }
    Write-Output 'Local test servers stopped; data retained.'
    exit
}
foreach ($binary in @("$pgBin/initdb.exe", $weed)) {
    if (!(Test-Path $binary)) { throw "Missing portable binary: $binary. See LOCAL_TEST.md." }
}
New-Item -ItemType Directory -Force "$local/logs", "$local/objects" | Out-Null
if (!(Test-Path $settingsPath)) {
    @{ password = (New-Secret); accessKey = 'neva-local-test'; secretKey = (New-Secret); sessionSecret = (New-Secret) } |
        ConvertTo-Json | Set-Content -LiteralPath $settingsPath -Encoding utf8NoBOM
}
$settings = Get-Content -LiteralPath $settingsPath -Raw | ConvertFrom-Json
# Set every connection option explicitly; never read the application's .env.
$env:PGHOST = '127.0.0.1'
$env:PGPORT = '55432'
$env:PGDATABASE = 'neva_local_test'
$env:PGUSER = 'neva_local_test'
$env:PGPASSWORD = $settings.password
$env:PGSSLMODE = 'disable'
$env:PGSERVICE = $null
$env:PGSERVICEFILE = $null
$env:PGPASSFILE = $null
$env:S3_ENDPOINT = 'http://127.0.0.1:18333'
$env:S3_REGION = 'us-east-1'
$env:S3_BUCKET = 'neva-local-test'
$env:S3_ACCESS_KEY_ID = $settings.accessKey
$env:S3_SECRET_ACCESS_KEY = $settings.secretKey
$env:SESSION_SECRET = $settings.sessionSecret
$env:NEVA_LOCAL_INTEGRATION = '1'

if (!(Test-Path "$data/PG_VERSION")) {
    $passwordFile = Join-Path $local 'pg-password'
    [IO.File]::WriteAllText($passwordFile, $settings.password)
    & "$pgBin/initdb.exe" -D $data -U neva_local_test -A scram-sha-256 --pwfile=$passwordFile --encoding=UTF8 --locale=C
    Check-Exit 'PostgreSQL initialization'
    Add-Content -LiteralPath "$data/postgresql.conf" -Value "`nlisten_addresses = '127.0.0.1'`nport = 55432"
}
if (!(Test-Path "$data/postmaster.pid")) {
    & "$pgBin/pg_ctl.exe" -D $data -l "$local/logs/postgres.log" -w start
    Check-Exit 'PostgreSQL start'
}
Wait-Port 55432
$exists = & "$pgBin/psql.exe" -d postgres -At -c "SELECT 1 FROM pg_database WHERE datname = 'neva_local_test'"
Check-Exit 'Database lookup'
if ($exists -ne '1') {
    & "$pgBin/createdb.exe" neva_local_test
    Check-Exit 'Test database creation'
}
$weedProcess = $null
if (Test-Path "$local/seaweed.pid") {
    $weedProcess = Get-Process -Id ([int](Get-Content "$local/seaweed.pid")) -ErrorAction SilentlyContinue
    if ($weedProcess -and $weedProcess.Path -ne $weed.Replace('/', '\')) { throw 'Saved S3 PID belongs to another executable.' }
}
if (!$weedProcess) {
    @{ identities = @(@{ name = 'local-test'; credentials = @(@{ accessKey = $settings.accessKey; secretKey = $settings.secretKey }); actions = @('Admin', 'Read', 'Write', 'List', 'Tagging') }) } |
        ConvertTo-Json -Depth 6 | Set-Content -LiteralPath "$local/s3.json" -Encoding utf8NoBOM
    $arguments = @('server', '-ip=127.0.0.1', '-ip.bind=127.0.0.1', '-s3', '-s3.ip.bind=127.0.0.1', '-s3.port=18333', '-s3.port.iceberg=0', '-s3.port.lance=0', '-master.port=19333', '-volume.port=18080', '-filer.port=18888', '-master.telemetry=false', '-master.volumeSizeLimitMB=64', "-dir=`"$local/objects`"", "-s3.config=`"$local/s3.json`"")
    $weedProcess = Start-Process -FilePath $weed -ArgumentList $arguments -WorkingDirectory "$local/objects" -WindowStyle Hidden -PassThru -RedirectStandardOutput "$local/logs/s3.out.log" -RedirectStandardError "$local/logs/s3.err.log"
    $weedProcess.Id | Set-Content "$local/seaweed.pid"
}
Wait-Port 18333
Write-Output 'Local PostgreSQL: 127.0.0.1:55432; S3: 127.0.0.1:18333. Credentials remain in ignored .local-test/settings.json.'
if ($Action -eq 'Test') {
    Push-Location $repo
    try {
        go test -tags=localintegration -run '^TestLocalIntegration$' -count=1 -v .
        Check-Exit 'Local integration tests'
    } finally { Pop-Location }
}
