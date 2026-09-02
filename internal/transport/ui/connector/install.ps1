$ErrorActionPreference = 'Stop'

$InstallRoot = Join-Path $env:LOCALAPPDATA 'S3Center\Connector'
$ProtocolRoot = 'HKCU:\Software\Classes\s3center'
$RequiredFiles = @('connector.mjs', 'launcher.cmd', 'package.json', 'package-lock.json', 'uninstall.ps1', 'README.md')

if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
    throw 'Node.js 20 or newer is required. Install Node.js, then run this installer again.'
}
if (-not (Get-Command npm.cmd -ErrorAction SilentlyContinue)) {
    throw 'npm was not found next to Node.js.'
}
$NodeMajor = [int]((& node --version).TrimStart('v').Split('.')[0])
if ($NodeMajor -lt 20) {
    throw 'Node.js 20 or newer is required.'
}

New-Item -ItemType Directory -Force -Path $InstallRoot | Out-Null
foreach ($File in $RequiredFiles) {
    $Source = Join-Path $PSScriptRoot $File
    if (-not (Test-Path -LiteralPath $Source)) {
        throw "Connector package is incomplete: $File is missing."
    }
    $Destination = Join-Path $InstallRoot $File
    if ((Resolve-Path -LiteralPath $Source).Path -ne [System.IO.Path]::GetFullPath($Destination)) {
        Copy-Item -LiteralPath $Source -Destination $Destination -Force
    }
}

Push-Location $InstallRoot
try {
    & npm.cmd ci --omit=dev --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
} finally {
    Pop-Location
}

New-Item -Path $ProtocolRoot -Force | Out-Null
Set-ItemProperty -Path $ProtocolRoot -Name '(Default)' -Value 'URL:S3 Center Cloudflare Connector'
Set-ItemProperty -Path $ProtocolRoot -Name 'URL Protocol' -Value ''
New-Item -Path "$ProtocolRoot\DefaultIcon" -Force | Out-Null
Set-ItemProperty -Path "$ProtocolRoot\DefaultIcon" -Name '(Default)' -Value 'msedge.exe,0'
New-Item -Path "$ProtocolRoot\shell\open\command" -Force | Out-Null
$Launcher = Join-Path $InstallRoot 'launcher.cmd'
Set-ItemProperty -Path "$ProtocolRoot\shell\open\command" -Name '(Default)' -Value ('"{0}" "%1"' -f $Launcher)

Write-Host ''
Write-Host 'S3 Center Connector installed for the current Windows user.' -ForegroundColor Green
Write-Host "Protocol: s3center://"
Write-Host "Profiles: $(Join-Path $env:LOCALAPPDATA 'S3Center\Profiles')"
Write-Host 'Return to S3 Center and click Open isolated Cloudflare login.'
