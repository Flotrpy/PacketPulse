# PacketPulse agent launcher for Windows.
#
# Run in PowerShell *as Administrator*:
#   irm https://packetpulse-test.onrender.com/v1/agent/download/start-ppagent.ps1 | iex
#
# It downloads the latest agent, lets traceroute replies through Windows
# Firewall, asks for your agent token once (and remembers it), then starts
# the agent. Keep the window open while you trace; Ctrl+C stops it.

$PPServer = "https://packetpulse-test.onrender.com"
$ErrorActionPreference = "Stop"

function Write-Step($text) { Write-Host "==> $text" -ForegroundColor Cyan }

# 1. Traceroute needs raw network sockets, which need Administrator.
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host ""
    Write-Host "PacketPulse needs PowerShell running as Administrator." -ForegroundColor Yellow
    Write-Host "Close this window, press Start, type PowerShell, right-click it,"
    Write-Host "choose 'Run as administrator', and run the same command again."
    return
}

# 2. Download the latest agent into %LOCALAPPDATA%\PacketPulse.
$dir = Join-Path $env:LOCALAPPDATA "PacketPulse"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$exe = Join-Path $dir "ppagent.exe"
Write-Step "Downloading the latest agent to $exe"
$tmp = "$exe.download"
& curl.exe -fsSL -o $tmp "$PPServer/v1/agent/download/ppagent-windows-amd64.exe"
if ($LASTEXITCODE -eq 0 -and (Test-Path $tmp)) {
    try { Move-Item -Force $tmp $exe }
    catch { Write-Host "Couldn't replace ppagent.exe (is another agent window still open?). Using the existing copy." -ForegroundColor Yellow; Remove-Item -Force $tmp -ErrorAction SilentlyContinue }
} elseif (Test-Path $exe) {
    Write-Host "Download failed; using the copy downloaded earlier." -ForegroundColor Yellow
} else {
    Write-Host "Download failed. Check your internet connection and try again." -ForegroundColor Red
    return
}

# 3. Allow ICMP replies (Time Exceeded, Echo Reply) through Windows Firewall, once.
$rule = "PacketPulse ICMP"
& netsh advfirewall firewall show rule name="$rule" | Out-Null
if ($LASTEXITCODE -ne 0) {
    Write-Step "Allowing traceroute replies through Windows Firewall (rule '$rule')"
    & netsh advfirewall firewall add rule name="$rule" dir=in action=allow protocol=icmpv4:any,any | Out-Null
}

# 4. Agent token: asked once, then remembered for your Windows user.
$token = $env:PACKETPULSE_AGENT_TOKEN
if (-not $token) { $token = [Environment]::GetEnvironmentVariable("PACKETPULSE_AGENT_TOKEN", "User") }
if (-not $token) {
    $token = (Read-Host "Paste your PacketPulse agent token").Trim()
    if (-not $token) { Write-Host "No token given." -ForegroundColor Red; return }
    [Environment]::SetEnvironmentVariable("PACKETPULSE_AGENT_TOKEN", $token, "User")
    Write-Step "Token saved for next time (user environment variable PACKETPULSE_AGENT_TOKEN)"
}
$env:PACKETPULSE_AGENT_TOKEN = $token

# 5. Run. The agent prints your location, then waits for traces from the globe.
Write-Host ""
Write-Host "Starting PacketPulse agent. Open https://packetpulse-eta.vercel.app and use 'Trace from my computer'." -ForegroundColor Green
Write-Host "Keep this window open. Press Ctrl+C to stop." -ForegroundColor Green
Write-Host ""
& $exe -server $PPServer
