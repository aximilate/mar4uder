#define _GNU_SOURCE
#include "agent_file_cmd.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <dirent.h>
#include <errno.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/socket.h>

#include <time.h>
#include <poll.h>

#define MAX_ACTIVE_TRANSFERS 4
#define MAX_ACTIVE_CMDS      4

static uint64_t current_time_ms_fallback(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static int is_safe_path(const char *path) {
    if (!path || path[0] == '\0') return 0;
    if (strstr(path, "/../") != NULL || strstr(path, "../") == path || strcmp(path, "..") == 0) {
        return 0;
    }
    return 1;
}

struct active_file_push {
    uint32_t transfer_id;
    int fd;
    uint64_t total_size;
    uint64_t written_size;
    char path[512];
    int in_use;
};

struct active_file_pull {
    uint32_t transfer_id;
    int fd;
    uint64_t offset;
    int in_use;
};

static struct active_file_push g_pushes[MAX_ACTIVE_TRANSFERS];
static struct active_file_pull g_pulls[MAX_ACTIVE_TRANSFERS];
static uint32_t g_seq = 1000;

static void send_packet(int sock_fd, uint8_t type, uint32_t session_id, const void *payload, size_t payload_len) {
    char buf[sizeof(struct packet_header) + MAX_PAYLOAD_SIZE];
    if (payload_len > MAX_PAYLOAD_SIZE) payload_len = MAX_PAYLOAD_SIZE;

    struct packet_header *hdr = (struct packet_header *)buf;
    hdr->magic = MAR4UDER_MAGIC;
    hdr->version = PROTOCOL_VERSION;
    hdr->type = type;
    hdr->flags = 0;
    hdr->session_id = session_id;
    hdr->seq = g_seq++;

    if (payload && payload_len > 0) {
        memcpy(buf + sizeof(*hdr), payload, payload_len);
    }
    send(sock_fd, buf, sizeof(*hdr) + payload_len, 0);
}

static void handle_fs_list(int sock_fd, const struct msg_fs_list_req *req, uint32_t session_id) {
    char dir_path[512];
    snprintf(dir_path, sizeof(dir_path), "%s", req->path[0] ? req->path : ".");

    DIR *d = opendir(dir_path);
    if (!d) {
        struct msg_fs_list_resp resp;
        memset(&resp, 0, sizeof(resp));
        resp.req_id = req->req_id;
        resp.status = 1; /* Error */
        resp.count = 0;
        resp.is_last = 1;
        send_packet(sock_fd, MSG_FS_LIST_RESP, session_id, &resp, sizeof(resp));
        return;
    }

    struct dirent *de;
    struct fs_entry entries[4];
    uint16_t batch_count = 0;

    char pkt_buf[sizeof(struct msg_fs_list_resp) + sizeof(entries)];

    while ((de = readdir(d)) != NULL) {
        if (strcmp(de->d_name, ".") == 0) continue;

        char full_entry_path[1024];
        snprintf(full_entry_path, sizeof(full_entry_path), "%s/%s", dir_path, de->d_name);

        struct stat st;
        memset(&st, 0, sizeof(st));
        stat(full_entry_path, &st);

        struct fs_entry *fe = &entries[batch_count++];
        memset(fe, 0, sizeof(*fe));
        fe->is_dir = S_ISDIR(st.st_mode) ? 1 : 0;
        fe->mode = (uint32_t)st.st_mode;
        fe->size = (uint64_t)st.st_size;
        snprintf(fe->name, sizeof(fe->name), "%s", de->d_name);

        if (batch_count >= 4) {
            struct msg_fs_list_resp *resp = (struct msg_fs_list_resp *)pkt_buf;
            resp->req_id = req->req_id;
            resp->status = 0;
            resp->count = batch_count;
            resp->is_last = 0;
            memcpy(pkt_buf + sizeof(*resp), entries, batch_count * sizeof(struct fs_entry));

            size_t total_len = sizeof(*resp) + batch_count * sizeof(struct fs_entry);
            send_packet(sock_fd, MSG_FS_LIST_RESP, session_id, pkt_buf, total_len);
            batch_count = 0;
        }
    }
    closedir(d);

    /* Send remaining entries or empty final packet */
    struct msg_fs_list_resp *resp = (struct msg_fs_list_resp *)pkt_buf;
    resp->req_id = req->req_id;
    resp->status = 0;
    resp->count = batch_count;
    resp->is_last = 1;
    if (batch_count > 0) {
        memcpy(pkt_buf + sizeof(*resp), entries, batch_count * sizeof(struct fs_entry));
    }
    size_t total_len = sizeof(*resp) + batch_count * sizeof(struct fs_entry);
    send_packet(sock_fd, MSG_FS_LIST_RESP, session_id, pkt_buf, total_len);
}

static void mkdir_p_for_file(const char *file_path) {
    char tmp[512];
    snprintf(tmp, sizeof(tmp), "%s", file_path);
    char *p = strrchr(tmp, '/');
    if (!p) return;
    *p = '\0';
    for (char *slash = tmp + 1; *slash; slash++) {
        if (*slash == '/') {
            *slash = '\0';
            mkdir(tmp, 0755);
            *slash = '/';
        }
    }
    mkdir(tmp, 0755);
}

static void handle_file_push_start(int sock_fd, const struct msg_file_push_start *req, uint32_t session_id) {
    if (!is_safe_path(req->path)) {
        struct msg_file_end end;
        end.transfer_id = req->transfer_id;
        end.status = 1; /* Error: unsafe path */
        send_packet(sock_fd, MSG_FILE_PUSH_END, session_id, &end, sizeof(end));
        return;
    }

    int slot = -1;
    for (int i = 0; i < MAX_ACTIVE_TRANSFERS; i++) {
        if (!g_pushes[i].in_use) {
            slot = i;
            break;
        }
    }

    if (slot < 0) {
        struct msg_file_end end;
        end.transfer_id = req->transfer_id;
        end.status = 1; /* Error: no slots */
        send_packet(sock_fd, MSG_FILE_PUSH_END, session_id, &end, sizeof(end));
        return;
    }

    mkdir_p_for_file(req->path);
    mode_t m = req->mode ? (mode_t)req->mode : 0644;
    int fd = open(req->path, O_WRONLY | O_CREAT | O_TRUNC, m);
    if (fd < 0) {
        struct msg_file_end end;
        end.transfer_id = req->transfer_id;
        end.status = 1; /* Error: cannot open */
        send_packet(sock_fd, MSG_FILE_PUSH_END, session_id, &end, sizeof(end));
        return;
    }

    g_pushes[slot].transfer_id = req->transfer_id;
    g_pushes[slot].fd = fd;
    g_pushes[slot].total_size = req->total_size;
    g_pushes[slot].written_size = 0;
    snprintf(g_pushes[slot].path, sizeof(g_pushes[slot].path), "%s", req->path);
    g_pushes[slot].in_use = 1;
}

static void handle_file_push_chunk(const struct msg_file_chunk *chunk, size_t payload_len) {
    if (payload_len < 14) return;
    size_t max_data = payload_len - 14;
    uint16_t clen = chunk->len;
    if (clen > max_data) clen = (uint16_t)max_data;
    if (clen > 1024) clen = 1024;
    if (clen == 0) return;

    for (int i = 0; i < MAX_ACTIVE_TRANSFERS; i++) {
        if (g_pushes[i].in_use && g_pushes[i].transfer_id == chunk->transfer_id) {
            ssize_t w = write(g_pushes[i].fd, chunk->data, clen);
            if (w > 0) {
                g_pushes[i].written_size += (uint64_t)w;
            }
            break;
        }
    }
}

static void handle_file_push_end(int sock_fd, const struct msg_file_end *req, uint32_t session_id) {
    for (int i = 0; i < MAX_ACTIVE_TRANSFERS; i++) {
        if (g_pushes[i].in_use && g_pushes[i].transfer_id == req->transfer_id) {
            fsync(g_pushes[i].fd);
            close(g_pushes[i].fd);
            g_pushes[i].in_use = 0;

            struct msg_file_end ack;
            ack.transfer_id = req->transfer_id;
            ack.status = 0; /* OK */
            send_packet(sock_fd, MSG_FILE_PUSH_END, session_id, &ack, sizeof(ack));
            break;
        }
    }
}

static void handle_file_pull_req(int sock_fd, const struct msg_file_pull_req *req, uint32_t session_id) {
    if (!is_safe_path(req->path)) {
        struct msg_file_end end;
        end.transfer_id = req->transfer_id;
        end.status = 1;
        send_packet(sock_fd, MSG_FILE_PULL_END, session_id, &end, sizeof(end));
        return;
    }

    int slot = -1;
    for (int i = 0; i < MAX_ACTIVE_TRANSFERS; i++) {
        if (!g_pulls[i].in_use) {
            slot = i;
            break;
        }
    }

    if (slot < 0) {
        struct msg_file_end end;
        end.transfer_id = req->transfer_id;
        end.status = 1;
        send_packet(sock_fd, MSG_FILE_PULL_END, session_id, &end, sizeof(end));
        return;
    }

    int fd = open(req->path, O_RDONLY);
    if (fd < 0) {
        struct msg_file_end end;
        end.transfer_id = req->transfer_id;
        end.status = 1;
        send_packet(sock_fd, MSG_FILE_PULL_END, session_id, &end, sizeof(end));
        return;
    }

    g_pulls[slot].transfer_id = req->transfer_id;
    g_pulls[slot].fd = fd;
    g_pulls[slot].offset = 0;
    g_pulls[slot].in_use = 1;

    /* Stream all chunks */
    struct msg_file_chunk chunk;
    chunk.transfer_id = req->transfer_id;

    while (1) {
        ssize_t n = read(fd, chunk.data, sizeof(chunk.data));
        if (n > 0) {
            chunk.offset = g_pulls[slot].offset;
            chunk.len = (uint16_t)n;
            send_packet(sock_fd, MSG_FILE_PULL_CHUNK, session_id, &chunk, sizeof(chunk) - sizeof(chunk.data) + n);
            g_pulls[slot].offset += (uint64_t)n;
        } else {
            break;
        }
    }

    close(fd);
    g_pulls[slot].in_use = 0;

    struct msg_file_end end;
    end.transfer_id = req->transfer_id;
    end.status = 0; /* Completed */
    send_packet(sock_fd, MSG_FILE_PULL_END, session_id, &end, sizeof(end));
}

static void handle_cmd_exec(int sock_fd, const struct msg_cmd_exec *req, uint32_t session_id) {
    int pfd[2];
    if (pipe(pfd) < 0) {
        struct msg_cmd_output out;
        memset(&out, 0, sizeof(out));
        out.req_id = req->req_id;
        out.exit_code = 127;
        out.len = 0;
        send_packet(sock_fd, MSG_CMD_OUTPUT, session_id, &out, sizeof(out));
        return;
    }

    int flags = fcntl(pfd[0], F_GETFL, 0);
    if (flags != -1) fcntl(pfd[0], F_SETFL, flags | O_NONBLOCK);

    pid_t pid = fork();
    if (pid == 0) {
        close(pfd[0]);
        dup2(pfd[1], STDOUT_FILENO);
        dup2(pfd[1], STDERR_FILENO);
        close(pfd[1]);

        /* Run isolated command via shell */
        execl("/bin/sh", "sh", "-c", req->command, (char *)NULL);
        _exit(127);
    }

    close(pfd[1]);

    uint32_t max_timeout_sec = (req->timeout_sec > 0 && req->timeout_sec <= 60) ? req->timeout_sec : 10;
    uint64_t start_ms = current_time_ms_fallback();
    uint64_t max_ms = (uint64_t)max_timeout_sec * 1000;

    struct msg_cmd_output out;
    memset(&out, 0, sizeof(out));
    out.req_id = req->req_id;
    out.exit_code = -1;

    char read_buf[sizeof(out.data)];
    int timed_out = 0;

    while (1) {
        struct pollfd fds;
        fds.fd = pfd[0];
        fds.events = POLLIN;
        int p_res = poll(&fds, 1, 100);

        if (p_res > 0 && (fds.revents & POLLIN)) {
            ssize_t n = read(pfd[0], read_buf, sizeof(read_buf));
            if (n > 0) {
                out.len = (uint16_t)n;
                memcpy(out.data, read_buf, n);
                send_packet(sock_fd, MSG_CMD_OUTPUT, session_id, &out, sizeof(out) - sizeof(out.data) + n);
            } else if (n == 0) {
                break;
            }
        }

        int status = 0;
        pid_t w_res = waitpid(pid, &status, WNOHANG);
        if (w_res > 0) {
            while (1) {
                ssize_t n = read(pfd[0], read_buf, sizeof(read_buf));
                if (n > 0) {
                    out.len = (uint16_t)n;
                    memcpy(out.data, read_buf, n);
                    send_packet(sock_fd, MSG_CMD_OUTPUT, session_id, &out, sizeof(out) - sizeof(out.data) + n);
                } else {
                    break;
                }
            }
            close(pfd[0]);
            int exit_code = WIFEXITED(status) ? WEXITSTATUS(status) : 1;
            out.exit_code = exit_code;
            out.len = 0;
            send_packet(sock_fd, MSG_CMD_OUTPUT, session_id, &out, sizeof(out));
            return;
        }

        if (current_time_ms_fallback() - start_ms >= max_ms) {
            timed_out = 1;
            kill(pid, SIGKILL);
            waitpid(pid, &status, 0);
            break;
        }
    }

    close(pfd[0]);
    out.exit_code = timed_out ? 124 : 1;
    out.len = 0;
    send_packet(sock_fd, MSG_CMD_OUTPUT, session_id, &out, sizeof(out));
}

int handle_file_cmd_packet(int sock_fd, const struct packet_header *hdr, const uint8_t *payload, size_t payload_len) {
    if (!hdr) return -1;

    switch (hdr->type) {
        case MSG_FS_LIST_REQ:
            if (payload_len >= sizeof(struct msg_fs_list_req)) {
                handle_fs_list(sock_fd, (const struct msg_fs_list_req *)payload, hdr->session_id);
                return 0;
            }
            break;

        case MSG_FILE_PUSH_START:
            if (payload_len >= sizeof(struct msg_file_push_start)) {
                handle_file_push_start(sock_fd, (const struct msg_file_push_start *)payload, hdr->session_id);
                return 0;
            }
            break;

        case MSG_FILE_PUSH_CHUNK:
            if (payload_len >= sizeof(struct msg_file_chunk) - 1024) {
                handle_file_push_chunk((const struct msg_file_chunk *)payload, payload_len);
                return 0;
            }
            break;

        case MSG_FILE_PUSH_END:
            if (payload_len >= sizeof(struct msg_file_end)) {
                handle_file_push_end(sock_fd, (const struct msg_file_end *)payload, hdr->session_id);
                return 0;
            }
            break;

        case MSG_FILE_PULL_REQ:
            if (payload_len >= sizeof(struct msg_file_pull_req)) {
                handle_file_pull_req(sock_fd, (const struct msg_file_pull_req *)payload, hdr->session_id);
                return 0;
            }
            break;

        case MSG_CMD_EXEC:
            if (payload_len >= sizeof(struct msg_cmd_exec)) {
                handle_cmd_exec(sock_fd, (const struct msg_cmd_exec *)payload, hdr->session_id);
                return 0;
            }
            break;

        default:
            break;
    }
    return -1;
}

void file_cmd_tick(int sock_fd) {
    (void)sock_fd;
}

void file_cmd_cleanup(void) {
    for (int i = 0; i < MAX_ACTIVE_TRANSFERS; i++) {
        if (g_pushes[i].in_use && g_pushes[i].fd >= 0) {
            close(g_pushes[i].fd);
            g_pushes[i].in_use = 0;
        }
        if (g_pulls[i].in_use && g_pulls[i].fd >= 0) {
            close(g_pulls[i].fd);
            g_pulls[i].in_use = 0;
        }
    }
}
