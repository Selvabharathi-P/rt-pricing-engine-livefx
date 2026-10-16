# Connects to the LiveFXHub WebSocket, authenticates, subscribes and prints
# raw messages. Works in Windows PowerShell 5.1 and PowerShell 7.
#
#   powershell -ExecutionPolicy Bypass -File .\scripts\upstream-ws.ps1
#   powershell -ExecutionPolicy Bypass -File .\scripts\upstream-ws.ps1 -Symbols EURUSD,XAUUSD -Count 50
#   ... -Count 0          # stream until Ctrl+C
#
# Credentials: -ApiKey/-Secret, else API_KEY/API_SECRET env vars, else .env.
# LiveFXHub allows ONE session per key: stop the engine first
# (docker compose stop pricing-engine) and start it again afterwards.
param(
    [string]$Url = 'wss://v3-prod.livefxhub.com/ws/algo',
    [string[]]$Symbols = @('EURUSD', 'XAUUSD', 'BTCUSD'),
    [int]$Count = 20,
    [string]$ApiKey = $env:API_KEY,
    [string]$Secret = $env:API_SECRET
)
$ErrorActionPreference = 'Stop'

# Fall back to .env in the repo root (one level above this script).
if (-not $ApiKey -or -not $Secret) {
    $envFile = Join-Path (Split-Path $PSScriptRoot -Parent) '.env'
    if (Test-Path $envFile) {
        foreach ($line in Get-Content $envFile) {
            if ($line -match '^\s*([A-Z_]+)\s*=\s*(.*)\s*$') {
                $v = $Matches[2].Trim('"', "'")
                if ($Matches[1] -eq 'API_KEY' -and -not $ApiKey) { $ApiKey = $v }
                if ($Matches[1] -eq 'API_SECRET' -and -not $Secret) { $Secret = $v }
            }
        }
    }
}
if (-not $ApiKey -or -not $Secret) { throw 'No credentials: pass -ApiKey/-Secret, set API_KEY/API_SECRET, or create .env' }
if ($Symbols.Count -eq 1 -and $Symbols[0].Contains(',')) { $Symbols = $Symbols[0].Split(',') }

$ws = New-Object System.Net.WebSockets.ClientWebSocket
$ct = [Threading.CancellationToken]::None
$buf = New-Object byte[] 65536

function Send-Json($obj) {
    $bytes = [Text.Encoding]::UTF8.GetBytes(($obj | ConvertTo-Json -Compress))
    $seg = New-Object 'System.ArraySegment[byte]' -ArgumentList (, $bytes)
    $ws.SendAsync($seg, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $ct).Wait()
}

function Receive-Text {
    $ms = New-Object IO.MemoryStream
    do {
        $seg = New-Object 'System.ArraySegment[byte]' -ArgumentList (, $buf)
        $r = $ws.ReceiveAsync($seg, $ct).Result
        if ($r.MessageType -eq [System.Net.WebSockets.WebSocketMessageType]::Close) {
            return "[server closed: $($r.CloseStatus) $($r.CloseStatusDescription)]"
        }
        $ms.Write($buf, 0, $r.Count)
    } while (-not $r.EndOfMessage)
    return [Text.Encoding]::UTF8.GetString($ms.ToArray()).TrimEnd()
}

try {
    Write-Host "connecting to $Url ..."
    $ws.ConnectAsync([Uri]$Url, $ct).Wait()

    Send-Json @{ action = 'auth'; api_key = $ApiKey; secret = $Secret }
    $reply = Receive-Text
    Write-Host "auth -> $reply"
    if ($reply -match 'error|closed') { return }

    Send-Json @{ action = 'subscribeMarketPrice'; symbols = $Symbols }
    Write-Host "subscribed: $($Symbols -join ', ')   (Ctrl+C to stop)"

    $n = 0
    while ($ws.State -eq [System.Net.WebSockets.WebSocketState]::Open -and ($Count -eq 0 -or $n -lt $Count)) {
        $msg = Receive-Text
        Write-Host ("{0:HH:mm:ss.fff}  {1}" -f (Get-Date), $msg)
        if ($msg.StartsWith('[server closed')) { break }
        $n++
    }
}
finally {
    # Always close cleanly so the upstream releases the session immediately.
    # (the server may already have closed it, e.g. after rejecting auth)
    try {
        if ($ws.State -eq [System.Net.WebSockets.WebSocketState]::Open -or
            $ws.State -eq [System.Net.WebSockets.WebSocketState]::CloseReceived) {
            $ws.CloseAsync([System.Net.WebSockets.WebSocketCloseStatus]::NormalClosure, 'bye', $ct).Wait()
        }
    } catch { }
    $ws.Dispose()
    Write-Host 'closed.'
}
