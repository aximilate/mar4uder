# MAR4UDER // Control Plane & Resilient Agent Engine

Комплекс удалённого администрирования, управления интерактивными псевдотерминалами (PTY) и стриминга рабочего стола (VNC / Framebuffer / WebRTC) через NAT и обратные туннели без внешних зависимостей.

---

## Архитектура системы

```
                 +-------------------------------------------------------+
                 |            MAR4UDER CORE (Go Daemon)                  |
                 |      - UDP Gateway (:443)  <- Heartbeats / PTY / RPC  |
                 |      - TCP Gateway (:443)  <- Direct Tile Stream      |
                 |      - REST Control API    <- /api/v1/nodes (:8080)   |
                 |      - Operator Console    <- TCP TUI (:9000)         |
                 |      - RFB Gateway         <- Standard VNC (:5900)    |
                 +-------------------------------------------------------+
                                     ^               ^
                      Бинарный туннель |               | Сессия оператора
                 (Heartbeat, PTY, RPC)|               | (CLI / TUI / REST / VNC)
                                     v               v
     +--------------------------------------+      +-----------------------------+
     |           mar4uder_agent             |      |       Оператор / Клиенты    |
     |  - Статический C-бинарник (Linux)    |      |  - Windows: .\m4r.ps1       |
     |  - Мультиплексированные PTY сессии   |      |  - Linux: m4r / nc / telnet |
     |  - Динамический X11 / dev-fb захват  |      |  - C-клиент: mar4uder_client|
     |  - Pixel-RLE компрессия тайлов       |      |  - Внешний VNC Viewer       |
     |  - WebRTC/RTSP стриминг (FFmpeg)     |      +-----------------------------+
     +--------------------------------------+
```

---

## Сетевые порты и протоколы

| Порт | Протокол | Назначение |
|---|---|---|
| `443` | **UDP** | Основной агентский туннель (регистрация, хартбиты, PTY, RPC) |
| `443` | **TCP** | Высокоскоростная передача dirty-тайлов экрана (Direct VNC stream) |
| `8080` | **TCP** | REST API v1, WebSocket-мосты, скрипты развертывания |
| `9000` | **TCP** | Интерактивная консоль оператора (TUI через `nc` / `telnet` / `m4r.ps1`) |
| `5900` | **TCP** | Стандартный RFB/VNC шлюз (RFC 6143) для внешних просмотрщиков |

---

## Быстрый старт

### 1. Сервер (Linux VPS)

```bash
# Развертывание сервера:
sudo bash install.sh

# Или ручная сборка ядра:
cd server
go build -o mar4uder-server .
./mar4uder-server
```

### 2. Запуск C-агента на целевой машине

```bash
# Сборка агента:
make linux

# Запуск с указанием сервера (или пула серверов для отказоустойчивости):
./bin/mar4uder_agent "relay.example.com:443" "my-node-01"

# С пулом failover-серверов:
./bin/mar4uder_agent "relay1.example.com:443,relay2.example.com:443" "my-node-01"
```

### 3. Управление оператора

#### Windows PowerShell CLI (`m4r.ps1`):
```powershell
# Список нод:
.\m4r.ps1 list

# Интерактивная TUI консоль (встроенный сокет без внешних утилит):
.\m4r.ps1

# Удаленное выполнение команды:
.\m4r.ps1 exec mos-ku1eet "uname -a && uptime"

# Просмотр файлов на ноде:
.\m4r.ps1 fs mos-ku1eet /var/log

# Скачивание файла с ноды:
.\m4r.ps1 get mos-ku1eet /etc/os-release ./os-release.txt

# Загрузка файла на ноду:
.\m4r.ps1 put mos-ku1eet ./script.sh /tmp/script.sh
```

#### Linux CLI (`m4r` / `nc`):
```bash
# Быстрая установка консольной утилиты:
curl -fsSL http://<SERVER_IP>:8080/m4r | bash

# Использование:
m4r list
m4r exec <node_id> "whoami"
m4r fs <node_id> /home
m4r
```

---

## Возможности агента (`mar4uder_agent`)

- **PTY Terminal Subsystem:** Создание независимых изолированных сессий через `forkpty()`, автонастройка цветовой гаммы `xterm-256color` и безопасное отключение bracketed paste без утечки временных файлов.
- **Failover & Dynamic Endpoint Switching:** Ротация серверов при обрыве связи и горячая смена хоста через триггер-файл `/tmp/.mar4uder_endpoint`.
- **Direct Screen Capture:** Автоматическое обнаружение X11 через `/proc/*/environ` и `/proc/*/cmdline`, динамическая загрузка `libX11.so.6` (`XGetImage`), фолбэк на `/dev/fb0` и виртуальный холст.
- **Input Injection:** Эмуляция перемещения указателя, кликов и нажатий клавиш через `libXtst.so.6` (`XTestFakeMotionEvent`, `XTestFakeKeyEvent`) и `/dev/uinput`.
- **Pixel-RLE 32-bit Compression:** Высокоэффективное сжатие тайлов экрана с автоматическим контролем переполнения буфера.
- **WebRTC / RTSP Low-Latency Stream:** Управление процессом H.264 кодирования (`ffmpeg` x11grab / VAAPI) с защитой от бесконечных циклов перезапуска.
- **Native File & Command RPC:** Потоковая передача файлов чанками по 1024 байта, рекурсивное создание каталогов, защищенная валидация путей (Path Traversal Protection) и неблокирующее исполнение команд с контролем таймаутов.

---

## Сборка проекта

```bash
# Сборка Linux-агента и клиента:
make linux

# Очистка артефактов сборки:
make clean
```

---

## REST API & Документация

Полная спецификация REST API v1, WebSocket-мостов и структуры пакетов M4RD доступна в файле [`API.md`](API.md).