# MAR4UDER // Control Plane & Relay API Specification

Спецификация программного интерфейса (REST API v1 и WebSocket Bridges) сервера **MAR4UDER**.

Позволяет управлять пулом узлов, открывать интерактивные терминальные сессии, стримить рабочий стол и встраивать управление в сторонние сервисы, дашборды или мобильные клиенты.

---

## 1. Общие сведения и сетевая модель

### Порты по умолчанию

| Порт / Протокол | Назначение | Описание |
|---|---|---|
| `8080 / TCP` | HTTP / WebSocket | Веб-интерфейс, REST API и WebSocket-мосты |
| `443 / UDP` | MAR4UDER Beacon / PTY | Агентский туннель (хартбиты, регистрация, терминал) |
| `443 / TCP` | MAR4UDER VNC Stream | Высокоскоростной поток dirty-тайлов экрана |

> **Сетевая оптимизация для строгих файрволов:**
> Весь трафик агента (UDP и TCP) по умолчанию работает через стандартный порт **443** (HTTPS). Это позволяет подключаться даже с машин в жестко изолированных корпоративных/гостевых сетях, где заблокированы нестандартные порты.

---

## 2. Аутентификация

Все запросы к REST API и WebSocket-соединения защищены общим Bearer-токеном (`auth_token` из `/etc/mar4uder/config.json`).

### Способы передачи токена:

1. **HTTP Header (рекомендуется для REST)**:
   ```http
   Authorization: Bearer <auth_token>
   ```

2. **Query Parameter (рекомендуется для WebSockets)**:
   ```
   ?token=<auth_token>
   ```

При отсутствии или неверном токене сервер возвращает HTTP `401 Unauthorized` с телом:
```json
{
  "error": "Unauthorized"
}
```

---

## 3. REST API Endpoints

### 3.1. Получение списка узлов

Возвращает список всех зарегистрированных агентов с текущим статусом онлайна.

- **Метод**: `GET`
- **Путь**: `/api/v1/nodes`
- **Заголовки**: `Authorization: Bearer <token>`

#### Пример ответа (`200 OK`):
```json
[
  {
    "id": "mos-pk2afz",
    "hostname": "workstation-01",
    "os_info": "Linux 6.6.87 x86_64",
    "remote_addr": "198.51.100.42:54812",
    "last_seen_ago_sec": 1,
    "is_online": true,
    "active_session_id": 1002,
    "cols": 120,
    "rows": 35,
    "vnc_active": false
  }
]
```

---

### 3.2. Получение детальной информации об узле

- **Метод**: `GET`
- **Путь**: `/api/v1/nodes/{id}`
- **Заголовки**: `Authorization: Bearer <token>`

#### Пример ответа (`200 OK`):
```json
{
  "id": "mos-pk2afz",
  "hostname": "workstation-01",
  "os_info": "Linux 6.6.87 x86_64",
  "remote_addr": "198.51.100.42:54812",
  "last_seen": "2026-09-28T19:40:00.123456Z",
  "active_session_id": 1002,
  "cols": 120,
  "rows": 35,
  "vnc_active": true,
  "vnc_width": 1920,
  "vnc_height": 1080,
  "vnc_bpp": 32
}
```

---

### 3.3. Открытие PTY-сессии (Терминал)

Отправляет команду на агент для инициализации свежего псевдотерминала (`/bin/bash` или `/bin/sh`). Если предыдущая сессия зависла, агент корректно пересоздает процесс с заданным размером окна.

- **Метод**: `POST`
- **Путь**: `/api/v1/nodes/{id}/pty/open`
- **Тело запроса (`application/json`)**:
```json
{
  "cols": 120,
  "rows": 40
}
```

#### Пример ответа (`200 OK`):
```json
{
  "status": "ok",
  "session_id": 1003
}
```

---

### 3.4. Закрытие сессии

Завершает активную сессию терминала на агенте с корректным освобождением ресурсов и гарантированным уничтожением дочерних процессов.

- **Метод**: `POST`
- **Путь**: `/api/v1/nodes/{id}/pty/close`

#### Пример ответа (`200 OK`):
```json
{
  "status": "closed"
}
```

---

### 3.5. Телеметрия сервера

- **Метод**: `GET`
- **Путь**: `/api/v1/stats`

#### Пример ответа (`200 OK`):
```json
{
  "start_time": "2026-09-28T18:00:00Z",
  "uptime_sec": 6023,
  "in_packets": 142050,
  "out_packets": 139880,
  "in_bytes": 15849200,
  "out_bytes": 48291000
}
```

---

## 4. WebSocket Bridges

### 4.1. Терминальный мост (`/ws/terminal`)

Обеспечивает полнодуплексную передачу потока PTY между веб-клиентом и агентом в реальном времени.

- **URL**: `ws://<HOST>:8080/ws/terminal?node={node_id}&token={token}`
- **Формат данных**:
  - **Сервер -> Клиент**: Текстовые или бинарные фреймы с сырым выводом терминала (поддерживаются ANSI-последовательности, цвета, курсор).
  - **Клиент -> Сервер**:
    - **Сырой ввод**: Нажатия клавиш, escape-коды стрелок, `Ctrl+C` (`\x03`), `Enter` (`\r`), etc.
    - **Команда изменения геометрии окна** (JSON-фрейм):
      ```json
      {
        "type": "resize",
        "cols": 140,
        "rows": 40
      }
      ```

#### Пример подключения на JavaScript:
```javascript
const ws = new WebSocket("ws://<SERVER_IP>:8080/ws/terminal?node=node_id&token=YOUR_TOKEN");

ws.onopen = () => {
  // Адаптировать размер под окно браузера
  ws.send(JSON.stringify({ type: "resize", cols: 100, rows: 30 }));
};

ws.onmessage = (event) => {
  // Вывод данных в терминал (например, xterm.js)
  term.write(event.data);
};

// Отправка ввода оператора
term.onData((data) => {
  ws.send(data);
});
```

---

### 4.2. Графический мост рабочего стола (`/ws/desktop`)

Транслирует видеопоток в виде изменившихся тайлов экрана (Dirty Tiles) и принимает события устройств ввода (мышь и клавиатура).

- **URL**: `ws://<HOST>:8080/ws/desktop?node={node_id}&token={token}`
- **Формат данных (Сервер -> Клиент)**:
  - **Фрейм инициализации разрешения (8 байт)**:
    - Байт 0-1: `0xFE 0xFE` (магический тег разрешения)
    - Байт 2-3: `width` (Big-Endian uint16)
    - Байт 4-5: `height` (Big-Endian uint16)
    - Байт 6: `bpp` (bits per pixel, обычно 32)
    - Байт 7: `reserved`
  - **Фрейм тайла (12 байт заголовок + полезная нагрузка)**:
    - Байт 0-1: `tx` (координата X тайла)
    - Байт 2-3: `ty` (координата Y тайла)
    - Байт 4-5: `cur_w` (ширина тайла)
    - Байт 6-7: `cur_h` (высота тайла)
    - Байт 8: `comp_flag` (`1` = сжатие Pixel-RLE, `0` = сырой RGBA32)
    - Байт 9: `reserved`
    - Байт 10-11: `payload_len` (длина данных)
    - Байт 12+: Байты пикселей (RLE или RGBA32)

- **Формат данных (Клиент -> Сервер) — Ввод оператора (11 байт)**:
  ```
  [0]    uint8   event_type   (1 = мышь, 2 = клавиша)
  [1]    uint8   button_mask  (бит 0: левая, бит 1: средняя, бит 2: правая)
  [2..3] uint16  x            (координата X курсора, Little-Endian)
  [4..5] uint16  y            (координата Y курсора, Little-Endian)
  [6..9] uint32  key_sym      (код клавиши / X11 KeySym, Little-Endian)
  [10]   uint8   down_flag    (1 = нажата, 0 = отпущена)
  ```

---

## 5. Динамическая смена эндпоинта агента на лету

Агент поддерживает бесшовную миграцию на новый сервер **прямо во время работы без перезапуска процесса**:

### Способ 1. Изнутри терминальной сессии (PTY)
Находясь в консоли управляемого хоста, достаточно записать новый адрес в файл-триггер:
```bash
echo "новый_сервер_vps:443" > /tmp/.mar4uder_endpoint
```
Агент в течение 1 секунды считает файл, удалит его и автоматически переключит UDP и TCP каналы на новый VPS.

### Способ 2. Через переменные окружения перед запуском
```bash
export MAR4UDER_RELAY="vps1.example.com:443,vps2.example.com:443"
export MAR4UDER_NODE="my-custom-node"
./mar4uder_agent
```

### Способ 3. Через конфигурационный файл
Записать список резервных адресов в `/etc/mar4uder.conf` или `./mar4uder.conf`:
```text
198.51.100.1:443,backup.relay.org:443
```

---

## 6. Система макросов (Macros API & CLI)

Макросы позволяют выполнять предустановленные и параметризованные системные команды на управляемых хостах. Доступны синхронно в NC CLI (`m <num> <macro> [args...]`) и в Web UI.

### 6.1. Список доступных макросов
- **Метод**: `GET /api/v1/macros`
- **Ответ**:
```json
[
  {
    "id": "poweroff",
    "name": "Power Off",
    "description": "Safely shutdown target machine",
    "template": "systemctl poweroff || poweroff || shutdown -h now",
    "parameters": []
  },
  {
    "id": "chromium",
    "name": "Chromium Browser URL",
    "description": "Launch Chromium with specified URL on GUI display :0",
    "template": "DISPLAY=:0 nohup chromium '{url}' >/dev/null 2>&1 &",
    "parameters": ["url"]
  }
]
```

### 6.2. Исполнение макроса на хосте
- **Метод**: `POST /api/v1/nodes/{node_id}/macro`
- **Тело запроса**:
```json
{
  "macro": "chromium",
  "args": ["https://google.com"]
}
```
- **Ответ**:
```json
{
  "status": "ok",
  "macro": "chromium",
  "command": "DISPLAY=:0 nohup chromium 'https://google.com' >/dev/null 2>&1 &"
}
```

---

## 7. Передача и приём файлов (File Transfer API & CLI)

Система поддерживает двустороннюю передачу файлов как в локальной сети (LAN), так и через глобальный интернет (WAN).

### 7.1. Отправка файла с панели администратора на компьютер (Upload)
- **NC CLI**: `put <num> <file_id_or_url> [remote_path]`
- **REST**: `POST /api/v1/nodes/{node_id}/upload` (Multipart Form)
  - Поля: `file` (бинарные данные), `target_path` (необязательно, по умолчанию `/tmp/<filename>`).
- Сервер сохраняет файл во временное хранилище и через PTY туннель отдаёт агенту команду на выгрузку и установку прав.

### 7.2. Запрос файла с компьютера на панель администратора (Download)
- **NC CLI**: `get <num> <remote_path>`
- **REST**: `POST /api/v1/nodes/{node_id}/download`
```json
{
  "remote_path": "/etc/os-release"
}
```
- Агент отправляет файл через multipart-запрос обратно на сервер, после чего файл становится доступен для скачивания в списке `/api/v1/files`.

### 7.3. Список и скачивание сохранённых файлов
- **Список**: `GET /api/v1/files`
- **Скачивание**: `GET /api/v1/files/{file_id}`

