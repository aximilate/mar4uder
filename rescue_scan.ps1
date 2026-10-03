# ====================================================================
#   M4R // ЛОКАЛЬНЫЙ СКАНЕР ВОССТАНОВЛЕНИЯ И АДМИНИСТРИРОВАНИЯ (WINDOWS)
#   Сканирует локальную сеть, находит машины с VNC/PTY
#   Позволяет по одной кнопке подключиться к Shell или открыть VNC
# ====================================================================

[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

function Get-LocalSubnet {
    $ip = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { 
        $_.IPAddress -notlike "127.*" -and $_.IPAddress -notlike "169.254.*" 
    } | Select-Object -First 1).IPAddress
    if ($ip) {
        $parts = $ip.Split('.')
        return "$($parts[0]).$($parts[1]).$($parts[2])"
    }
    return "192.168.1"
}

function Scan-Subnet {
    param([string]$SubnetPrefix)
    Clear-Host
    Write-Host "====================================================================" -ForegroundColor Cyan
    Write-Host "      M4R // ЛОКАЛЬНЫЙ СКАНЕР ВОССТАНОВЛЕНИЯ И СЕТИ (WINDOWS)       " -ForegroundColor Green
    Write-Host "====================================================================" -ForegroundColor Cyan
    Write-Host "[*] Сканирование подсети: $($SubnetPrefix).0/24 ..."
    Write-Host "[*] Поиск открытых портов VNC (5900/5901) и PTY/Shell (9001/9000/22)...`n"

    $results = [System.Collections.Generic.List[PSCustomObject]]::new()
    $jobs = @()

    $scriptBlock = {
        param($ip)
        $vncPort = 0
        $ptyPort = 0

        # Check 5900
        $tcp = New-Object System.Net.Sockets.TcpClient
        $iar = $tcp.BeginConnect($ip, 5900, $null, $null)
        if ($iar.AsyncWaitHandle.WaitOne(200)) {
            try { $tcp.EndConnect($iar); $vncPort = 5900 } catch {}
        }
        $tcp.Close()

        # Check 5901
        if ($vncPort -eq 0) {
            $tcp = New-Object System.Net.Sockets.TcpClient
            $iar = $tcp.BeginConnect($ip, 5901, $null, $null)
            if ($iar.AsyncWaitHandle.WaitOne(200)) {
                try { $tcp.EndConnect($iar); $vncPort = 5901 } catch {}
            }
            $tcp.Close()
        }

        # Check PTY ports
        foreach ($p in 9001, 9000, 22) {
            $tcp = New-Object System.Net.Sockets.TcpClient
            $iar = $tcp.BeginConnect($ip, $p, $null, $null)
            if ($iar.AsyncWaitHandle.WaitOne(200)) {
                try { $tcp.EndConnect($iar); $ptyPort = $p; $tcp.Close(); break } catch {}
            }
            $tcp.Close()
        }

        if ($vncPort -gt 0 -or $ptyPort -gt 0) {
            [PSCustomObject]@{
                IP = $ip
                VNCPort = $vncPort
                PTYPort = $ptyPort
            }
        }
    }

    # Parallel run using RunspacePool
    $RunspacePool = [runspacefactory]::CreateRunspacePool(1, 64)
    $RunspacePool.Open()
    $tasks = @()

    1..254 | ForEach-Object {
        $ip = "$SubnetPrefix.$_"
        $ps = [powershell]::Create().AddScript($scriptBlock).AddArgument($ip)
        $ps.RunspacePool = $RunspacePool
        $tasks += [PSCustomObject]@{
            Pipe = $ps
            Async = $ps.BeginInvoke()
        }
    }

    foreach ($t in $tasks) {
        $res = $t.Pipe.EndInvoke($t.Async)
        $t.Pipe.Dispose()
        if ($res) {
            $results.Add($res[0])
        }
    }
    $RunspacePool.Close()
    $RunspacePool.Dispose()

    return $results
}

$subnet = Get-LocalSubnet
$found = Scan-Subnet -SubnetPrefix $subnet

if ($found.Count -eq 0) {
    Write-Host "  [-] В подсети $subnet.0/24 не найдено активных машин с VNC/PTY.`n" -ForegroundColor Yellow
} else {
    Write-Host "  #   IP АДРЕС            VNC ЭКРАН          PTY SHELL          СТАТУС" -ForegroundColor White
    Write-Host "  ----------------------------------------------------------------------"
    $idx = 1
    foreach ($m in $found) {
        $vncStr = if ($m.VNCPort -gt 0) { ":$($m.VNCPort) (VNC READY)" } else { "нет" }
        $ptyStr = if ($m.PTYPort -gt 0) { ":$($m.PTYPort) (Shell)" } else { "нет" }
        Write-Host ("  [{0}] {1,-19} {2,-18} {3,-18} ONLINE" -f $idx, $m.IP, $vncStr, $ptyStr)
        $idx++
    }
    Write-Host "  ----------------------------------------------------------------------`n"
}

while ($true) {
    Write-Host "Действия:" -ForegroundColor Yellow
    Write-Host "  - Введите <номер> (напр. '1') : прямое подключение к Shell выбранного компьютера"
    Write-Host "  - Введите v <номер> (напр. 'v 1') : запустить VNC Viewer"
    Write-Host "  - Введите r : повторить сканирование | q: выход`n"

    $cmd = (Read-Host "scanner").Trim()
    if ($cmd -in "q", "quit", "exit") { break }
    if ($cmd -in "r", "scan") {
        $found = Scan-Subnet -SubnetPrefix $subnet
        continue
    }

    if ($cmd -match "^v\s+(\d+)$") {
        $num = [int]$matches[1]
        if ($num -ge 1 -and $num -le $found.Count) {
            $target = $found[$num - 1]
            if ($target.VNCPort -gt 0) {
                Write-Host "[*] Запуск VNC на $($target.IP):$($target.VNCPort)..." -ForegroundColor Cyan
                Start-Process "vncviewer.exe" "$($target.IP):$($target.VNCPort)" -ErrorAction SilentlyContinue
            } else {
                Write-Host "[-] На выбранной машине не открыт VNC порт." -ForegroundColor Red
            }
        }
        continue
    }

    if ($cmd -match "^\d+$") {
        $num = [int]$cmd
        if ($num -ge 1 -and $num -le $found.Count) {
            $target = $found[$num - 1]
            $p = if ($target.PTYPort -gt 0) { $target.PTYPort } else { 9001 }
            Write-Host "[+] Подключение к PTY Shell $($target.IP):$p ..." -ForegroundColor Green
            python nc.py $target.IP $p
        } else {
            Write-Host "[-] Неверный номер машины." -ForegroundColor Red
        }
        continue
    }
}
