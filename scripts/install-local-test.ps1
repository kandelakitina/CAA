#Requires -Version 7.2
# Portable Windows test dependencies only. Does not install services or edit PATH.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$repo = Split-Path $PSScriptRoot -Parent
$local = Join-Path $repo '.local-test'
New-Item -ItemType Directory -Force "$local/downloads" | Out-Null
$packages = @(
    @{ Name = 'postgresql-17.6.zip'; Url = 'https://get.enterprisedb.com/postgresql/postgresql-17.6-1-windows-x64-binaries.zip'; Hash = 'd378882abd001a186735acd6f6ba716bca6ccd192e800412d4fd15ed25376b3e'; Destination = 'postgresql'; Binary = 'postgresql/pgsql/bin/initdb.exe' },
    @{ Name = 'seaweedfs-4.46.zip'; Url = 'https://github.com/seaweedfs/seaweedfs/releases/download/4.46/windows_amd64.zip'; Hash = 'd89d62fb56595f9ad2f5e62fc76943cded478cf40d0fe69868b52488a8d1d339'; Destination = 'seaweedfs'; Binary = 'seaweedfs/weed.exe' }
)
foreach ($package in $packages) {
    $archive = Join-Path "$local/downloads" $package.Name
    if (!(Test-Path $archive)) { Invoke-WebRequest -Uri $package.Url -OutFile $archive }
    if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $package.Hash) {
        throw "Archive checksum mismatch: $($package.Name). Do not execute it."
    }
    if (!(Test-Path (Join-Path $local $package.Binary))) {
        Expand-Archive -LiteralPath $archive -DestinationPath (Join-Path $local $package.Destination)
    }
}
Write-Output 'Portable dependencies verified. Run ./scripts/local-test.ps1 -Action Test.'
