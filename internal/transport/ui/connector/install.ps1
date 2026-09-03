$ErrorActionPreference = 'Stop'

$InstallRoot = Join-Path $env:LOCALAPPDATA 'S3Center\Connector'
$ProtocolRoot = 'HKCU:\Software\Classes\s3center'
$RequiredFiles = @(
    'connector.mjs',
    'launcher.cmd',
    'package.json',
    'package-lock.json',
    'uninstall.ps1',
    'README.md'
)

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

$ChromeCandidates = @()
foreach ($Root in @($env:PROGRAMFILES, ${env:PROGRAMFILES(X86)}, $env:LOCALAPPDATA)) {
    if ($Root) { $ChromeCandidates += (Join-Path $Root 'Google\Chrome\Application\chrome.exe') }
}
$ChromePath = $ChromeCandidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
if (-not $ChromePath) {
    throw 'Google Chrome is required. Connector 2.1 intentionally does not use Edge because Windows implicit sign-in can attach the local Microsoft account to a new Edge profile.'
}

New-Item -ItemType Directory -Force -Path $InstallRoot | Out-Null
foreach ($File in $RequiredFiles) {
    $Source = Join-Path $PSScriptRoot $File
    if (-not (Test-Path -LiteralPath $Source)) {
        throw "Connector package is incomplete: $File is missing."
    }
    $Destination = Join-Path $InstallRoot $File
    $DestinationDirectory = Split-Path -Parent $Destination
    New-Item -ItemType Directory -Force -Path $DestinationDirectory | Out-Null
    if ([System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $Source).Path) -ne [System.IO.Path]::GetFullPath($Destination)) {
        Copy-Item -LiteralPath $Source -Destination $Destination -Force
    }
}

# Connector 2.1 uses a loopback control page because current stable Chrome no
# longer accepts command-line installation of unpacked extensions. This folder
# contains only obsolete Connector code; browser profiles live elsewhere.
$ObsoleteExtensionRoot = Join-Path $InstallRoot 'extension'
if (Test-Path -LiteralPath $ObsoleteExtensionRoot) {
    Remove-Item -LiteralPath $ObsoleteExtensionRoot -Recurse -Force
}

Push-Location $InstallRoot
try {
    # npm ci also removes the obsolete Playwright 1.x dependency while keeping
    # only the small local proxy relay used for authenticated proxy URLs.
    & npm.cmd ci --omit=dev --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed.' }
} finally {
    Pop-Location
}

New-Item -Path $ProtocolRoot -Force | Out-Null
Set-ItemProperty -Path $ProtocolRoot -Name '(Default)' -Value 'URL:S3 Center Cloudflare Connector'
Set-ItemProperty -Path $ProtocolRoot -Name 'URL Protocol' -Value ''
New-Item -Path "$ProtocolRoot\DefaultIcon" -Force | Out-Null
Set-ItemProperty -Path "$ProtocolRoot\DefaultIcon" -Name '(Default)' -Value ('"{0}",0' -f $ChromePath)
New-Item -Path "$ProtocolRoot\shell\open\command" -Force | Out-Null
$Launcher = Join-Path $InstallRoot 'launcher.cmd'
Set-ItemProperty -Path "$ProtocolRoot\shell\open\command" -Name '(Default)' -Value ('"{0}" "%1"' -f $Launcher)

Write-Host ''
Write-Host 'S3 Center Connector 2.1 installed for the current Windows user.' -ForegroundColor Green
Write-Host 'Cloudflare login now uses a fresh unsigned native Chrome profile with sync, import, extensions, and password storage disabled.' -ForegroundColor Green
Write-Host "Protocol: s3center://"
Write-Host "Profiles: $(Join-Path $env:LOCALAPPDATA 'S3Center\Profiles')\<profile-id>\browser-chrome-v1"
Write-Host 'Return to S3 Center and click Open isolated Cloudflare login.'
