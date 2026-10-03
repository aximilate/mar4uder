#!/usr/bin/env bash
# ====================================================================
#   M4R // ЛОКАЛЬНЫЙ СКАНЕР ВОССТАНОВЛЕНИЯ И АДМИНИСТРИРОВАНИЯ
#   Сканирует локальную сеть, находит машины с VNC/PTY
#   Позволяет по одной кнопке войти в PTY Shell или запустить VNC
# ====================================================================

set -e

# Detect local IP and subnet
LOCAL_IP=$(ip -o -f inet addr show 2>/dev/null | awk '/scope global/ {print $4}' | head -n 1 | cut -d'/' -f1)
if [ -z "$LOCAL_IP" ]; then
    LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
fi
SUBNET_PREFIX=$(echo "$LOCAL_IP" | cut -d'.' -f1-3)

if [ -z "$SUBNET_PREFIX" ]; then
    echo -e "\033[31m[-] Не удалось автоматически определить локальную сеть.\033[0m"
    exit 1
fi

scan_network() {
    clear 2>/dev/null || true
    echo -e "\033[1;36m====================================================================\033[0m"
    echo -e "\033[1;32m      M4R // ЛОКАЛЬНЫЙ СКАНЕР ВОССТАНОВЛЕНИЯ И СЕТИ                 \033[0m"
    echo -e "\033[1;36m====================================================================\033[0m"
    echo -e "[*] Локальный IP: \033[1m$LOCAL_IP\033[0m | Сканирование подсети: \033[1m${SUBNET_PREFIX}.0/24\033[0m ..."
    echo -e "[*] Поиск активных VNC (5900/5901) и PTY/Shell сервисов...\n"

    SCAN_FILE="/tmp/m4r_scan_results.$$"
    rm -f "$SCAN_FILE"

    python3 -c "
import socket, concurrent.futures

subnet = '$SUBNET_PREFIX'
def check(i):
    ip = f'{subnet}.{i}'
    vnc_port = 0
    pty_port = 0
    # Check 5900
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(0.25)
        if s.connect_ex((ip, 5900)) == 0:
            vnc_port = 5900
        s.close()
    except: pass

    # Check 5901
    if not vnc_port:
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(0.25)
            if s.connect_ex((ip, 5901)) == 0:
                vnc_port = 5901
            s.close()
        except: pass

    # Check PTY ports 9001, 9000, 22
    for p in (9001, 9000, 22):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(0.25)
            if s.connect_ex((ip, p)) == 0:
                pty_port = p
                s.close()
                break
            s.close()
        except: pass

    if vnc_port or pty_port:
        return (ip, vnc_port, pty_port)
    return None

with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
    futs = [ex.submit(check, i) for i in range(1, 255)]
    with open('$SCAN_FILE', 'w') as f:
        for fut in futs:
            res = fut.result()
            if res:
                f.write(f'{res[0]}|{res[1]}|{res[2]}\n')
" 2>/dev/null || true

    FOUND_IPS=()
    FOUND_VNCS=()
    FOUND_PTYS=()

    if [ -f "$SCAN_FILE" ] && [ -s "$SCAN_FILE" ]; then
        echo -e "  \033[1m#   IP АДРЕС            VNC ЭКРАН          PTY SHELL          СТАТУС\033[0m"
        echo -e "  ----------------------------------------------------------------------"
        idx=1
        while IFS='|' read -r ip vnc pty; do
            FOUND_IPS+=("$ip")
            FOUND_VNCS+=("$vnc")
            FOUND_PTYS+=("$pty")
            vnc_str="нет"
            pty_str="нет"
            [ "$vnc" -gt 0 ] 2>/dev/null && vnc_str=":${vnc} (VNC READY)"
            [ "$pty" -gt 0 ] 2>/dev/null && pty_str=":${pty} (Shell)"
            printf "  [\033[1m%%d\033[0m] %%-19s \033[1;35m%%-18s\033[0m \033[1;32m%%-18s\033[0m ONLINE\n" "$idx" "$ip" "$vnc_str" "$pty_str"
            ((idx++))
        done < "$SCAN_FILE"
        echo -e "  ----------------------------------------------------------------------\n"
    else
        echo -e "  \033[33m[-] В подсети ${SUBNET_PREFIX}.0/24 не найдено активных машин с VNC/PTY.\033[0m\n"
    fi
    rm -f "$SCAN_FILE"
}

scan_network

while true; do
    echo -e "\033[1;33mДействия:\033[0m"
    echo -e "  - Введите \033[1m<номер>\033[0m (напр. '1') : прямое подключение к Shell выбранного компьютера"
    echo -e "  - Введите \033[1mv <номер>\033[0m (напр. 'v 1') : запустить VNC Viewer для просмотра рабочего стола"
    echo -e "  - Введите \033[1mr\033[0m : повторить сканирование сети | \033[1mq\033[0m: выход\n"
    read -rp "scanner> " cmd

    if [ "$cmd" = "q" ] || [ "$cmd" = "quit" ] || [ "$cmd" = "exit" ]; then
        echo "Выход."
        break
    fi

    if [ "$cmd" = "r" ] || [ "$cmd" = "scan" ]; then
        scan_network
        continue
    fi

    if [[ "$cmd" =~ ^v[[:space:]]+([0-9]+)$ ]]; then
        num="${BASH_REMATCH[1]}"
        idx=$((num - 1))
        target_ip="${FOUND_IPS[$idx]}"
        target_vnc="${FOUND_VNCS[$idx]}"
        if [ -n "$target_ip" ] && [ "$target_vnc" -gt 0 ]; then
            echo -e "\033[1;36m[*] Запуск VNC клиента на ${target_ip}:${target_vnc} ...\033[0m"
            if command -v vncviewer >/dev/null 2>&1; then
                nohup vncviewer "${target_ip}:${target_vnc}" >/dev/null 2>&1 &
            elif command -v remmina >/dev/null 2>&1; then
                nohup remmina -c "vnc://${target_ip}:${target_vnc}" >/dev/null 2>&1 &
            else
                echo -e "[-] VNC-клиент не найден в системе. Подключитесь вручную: ${target_ip}:${target_vnc}"
            fi
        else
            echo -e "\033[31m[-] Неверный номер или на машине не открыт VNC порт.\033[0m"
        fi
        continue
    fi

    if [[ "$cmd" =~ ^[0-9]+$ ]]; then
        idx=$((cmd - 1))
        target_ip="${FOUND_IPS[$idx]}"
        target_pty="${FOUND_PTYS[$idx]}"
        if [ -n "$target_ip" ]; then
            echo -e "\033[1;32m[+] Подключение к PTY Shell на ${target_ip}... (Для выхода наберите exit или Ctrl+C)\033[0m\n"
            if [ "$target_pty" -eq 9001 ] || [ "$target_pty" -eq 9000 ]; then
                nc "${target_ip}" "${target_pty}" || telnet "${target_ip}" "${target_pty}"
            elif [ "$target_pty" -eq 22 ]; then
                ssh "${target_ip}"
            else
                nc "${target_ip}" 9001 || nc "${target_ip}" 9000 || ssh "${target_ip}"
            fi
        else
            echo -e "\033[31m[-] Компьютер с номером $cmd не найден.\033[0m"
        fi
        continue
    fi
done
