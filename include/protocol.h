#ifndef MAR4UDER_PROTOCOL_H
#define MAR4UDER_PROTOCOL_H

#include <stdint.h>

#define MAR4UDER_MAGIC 0x4D345244 /* "M4RD" */
#define PROTOCOL_VERSION 1
#define MAX_PAYLOAD_SIZE 1400

/* Message types */
enum msg_type {
    MSG_HEARTBEAT    = 0x01, /* Keepalive ping/pong */
    MSG_REGISTER     = 0x02, /* Node registration on relay */
    MSG_SESSION_OPEN = 0x03, /* Server requests node to open/attach PTY */
    MSG_PTY_DATA     = 0x04, /* Raw terminal data (both directions) */
    MSG_PTY_RESIZE   = 0x05, /* Terminal window resize (cols, rows) */
    MSG_SESSION_CLOSE= 0x06, /* Detach/close session */

    /* VNC / Remote Desktop Stream */
    MSG_VNC_START    = 0x10, /* Request VNC stream start */
    MSG_VNC_STOP     = 0x11, /* Request VNC stream stop */
    MSG_VNC_DATA     = 0x12, /* RFB / Framebuffer data chunk */
    MSG_VNC_INPUT    = 0x13, /* Mouse & Keyboard input events */

    /* Native File & Command Operations (Zero external dependencies) */
    MSG_CMD_EXEC        = 0x20, /* Execute single isolated command */
    MSG_CMD_OUTPUT      = 0x21, /* Output chunk / exit code */
    MSG_FS_LIST_REQ     = 0x22, /* Request directory contents */
    MSG_FS_LIST_RESP    = 0x23, /* Directory entries payload */
    MSG_FILE_PUSH_START = 0x24, /* Server -> Node upload start */
    MSG_FILE_PUSH_CHUNK = 0x25, /* Server -> Node data chunk */
    MSG_FILE_PUSH_END   = 0x26, /* Server -> Node upload complete */
    MSG_FILE_PULL_REQ   = 0x27, /* Server -> Node download request */
    MSG_FILE_PULL_CHUNK = 0x28, /* Node -> Server data chunk */
    MSG_FILE_PULL_END   = 0x29, /* Node -> Server download complete */

    /* WebRTC / Ultra-Low-Latency Stream Transport */
    MSG_STREAM_START    = 0x30, /* Request stream start (RTSP/WebRTC) */
    MSG_STREAM_STOP     = 0x31, /* Request stream stop */
    MSG_STREAM_STATUS   = 0x32  /* Stream status/telemetry */
};

#pragma pack(push, 1)

/* Common packet header for transport (16 bytes) */
struct packet_header {
    uint32_t magic;         /* MAR4UDER_MAGIC */
    uint8_t  version;       /* PROTOCOL_VERSION */
    uint8_t  type;          /* enum msg_type */
    uint16_t flags;         /* Channel / Flags */
    uint32_t session_id;    /* Session ID */
    uint32_t seq;           /* Packet sequence number */
};

/* Payload for MSG_REGISTER */
struct msg_register {
    char node_id[64];       /* Unique identifier/name of node */
    char hostname[64];      /* OS hostname */
    char os_info[64];       /* Linux/Windows kernel info */
    uint16_t screen_w;      /* Native screen width (pixels) */
    uint16_t screen_h;      /* Native screen height (pixels) */
};

/* Payload for MSG_PTY_RESIZE */
struct msg_resize {
    uint16_t cols;
    uint16_t rows;
};

/* Event types for MSG_VNC_INPUT */
#define VNC_EVENT_POINTER 0
#define VNC_EVENT_KEY     1

/* Payload for MSG_VNC_INPUT */
struct msg_vnc_input {
    uint8_t  event_type;    /* 0 = pointer/mouse, 1 = key (VNC_EVENT_*) */
    uint8_t  button_mask;   /* mouse buttons (1=left, 2=middle, 4=right, 8=wheelup, 16=wheeldown) */
    uint16_t x;             /* pointer X (normalized 0..65535) */
    uint16_t y;             /* pointer Y (normalized 0..65535) */
    uint32_t key_sym;       /* X11 key symbol */
    uint8_t  down_flag;     /* 1 = pressed, 0 = released */
};

/* Payload for MSG_CMD_EXEC */
struct msg_cmd_exec {
    uint32_t req_id;
    uint32_t timeout_sec;
    char     command[512];
};

/* Payload for MSG_CMD_OUTPUT */
struct msg_cmd_output {
    uint32_t req_id;
    int32_t  exit_code;     /* -1 while running, >= 0 when finished */
    uint16_t len;
    char     data[1024];
};

/* Payload for MSG_FS_LIST_REQ */
struct msg_fs_list_req {
    uint32_t req_id;
    char     path[512];
};

/* Item inside MSG_FS_LIST_RESP */
struct fs_entry {
    uint8_t  is_dir;
    uint32_t mode;
    uint64_t size;
    char     name[256];
};

/* Payload for MSG_FS_LIST_RESP */
struct msg_fs_list_resp {
    uint32_t req_id;
    uint8_t  status;        /* 0 = ok, 1 = access error */
    uint16_t count;         /* number of fs_entry items following */
    uint8_t  is_last;       /* 1 if last chunk */
};

/* Payload for MSG_FILE_PUSH_START */
struct msg_file_push_start {
    uint32_t transfer_id;
    uint64_t total_size;
    uint32_t mode;
    char     path[512];
};

/* Payload for MSG_FILE_PUSH_CHUNK and MSG_FILE_PULL_CHUNK */
struct msg_file_chunk {
    uint32_t transfer_id;
    uint64_t offset;
    uint16_t len;
    uint8_t  data[1024];
};

/* Payload for MSG_FILE_PUSH_END and MSG_FILE_PULL_END */
struct msg_file_end {
    uint32_t transfer_id;
    uint8_t  status;        /* 0 = success, 1 = error */
};

/* Payload for MSG_FILE_PULL_REQ */
struct msg_file_pull_req {
    uint32_t transfer_id;
    char     path[512];
};

/* Payload for MSG_STREAM_START */
struct msg_stream_start {
    char     server_host[64];
    uint16_t rtsp_port;
    char     stream_name[64];
    uint16_t width;      /* Target width (0 = auto/native) */
    uint16_t height;     /* Target height (0 = auto/native) */
    uint16_t fps;        /* Framerate (0 = 30) */
    uint16_t bitrate_kb; /* Bitrate in kbps (0 = 2000) */
    uint8_t  preset;     /* 0 = ultrafast, 1 = superfast, 2 = veryfast */
    uint8_t  reserved[7];
};

#pragma pack(pop)

#endif /* MAR4UDER_PROTOCOL_H */
