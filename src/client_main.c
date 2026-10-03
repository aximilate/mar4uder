#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <termios.h>
#include <sys/ioctl.h>
#include <signal.h>
#include <poll.h>
#include <errno.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <netdb.h>

static struct termios orig_termios;
static int raw_mode_active = 0;
static volatile sig_atomic_t g_resized = 0;
static int g_is_attached = 0;

static void restore_terminal(void) {
    if (raw_mode_active) {
        tcsetattr(STDIN_FILENO, TCSAFLUSH, &orig_termios);
        raw_mode_active = 0;
    }
}

static void enable_raw_mode(void) {
    if (!isatty(STDIN_FILENO)) return;
    if (tcgetattr(STDIN_FILENO, &orig_termios) < 0) return;
    atexit(restore_terminal);

    struct termios raw = orig_termios;
    cfmakeraw(&raw);
    /* Allow local signals like Ctrl-C to be passed raw through socket */
    raw.c_cc[VMIN] = 1;
    raw.c_cc[VTIME] = 0;

    tcsetattr(STDIN_FILENO, TCSAFLUSH, &raw);
    raw_mode_active = 1;
}

static void sigwinch_handler(int sig) {
    (void)sig;
    g_resized = 1;
}

static void sigexit_handler(int sig) {
    (void)sig;
    restore_terminal();
    _exit(0);
}

static void send_window_size(int sock_fd) {
    struct winsize ws;
    if (ioctl(STDIN_FILENO, TIOCGWINSZ, &ws) == 0 && ws.ws_col > 0 && ws.ws_row > 0) {
        char cmd[64];
        int len = snprintf(cmd, sizeof(cmd), "/size %u %u\n", ws.ws_col, ws.ws_row);
        if (len > 0) {
            send(sock_fd, cmd, len, 0);
        }
    }
}

int main(int argc, char *argv[]) {
    const char *env_host = getenv("MAR4UDER_HOST");
    const char *host = (argc >= 2) ? argv[1] : (env_host ? env_host : "127.0.0.1");
    const char *port = (argc >= 3) ? argv[2] : "9000";

    if (argc >= 2 && (strcmp(argv[1], "-h") == 0 || strcmp(argv[1], "--help") == 0)) {
        printf("Usage: %s [relay_host] [tcp_port]\nDefault: 127.0.0.1 9000\n", argv[0]);
        return 0;
    }

    struct addrinfo hints, *res = NULL;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;

    int err = getaddrinfo(host, port, &hints, &res);
    if (err != 0 || !res) {
        fprintf(stderr, "[-] Failed to resolve %s:%s\n", host, port);
        return 1;
    }

    int sock_fd = socket(res->ai_family, res->ai_socktype, res->ai_protocol);
    if (sock_fd < 0) {
        perror("[-] socket");
        freeaddrinfo(res);
        return 1;
    }

    if (connect(sock_fd, res->ai_addr, res->ai_addrlen) < 0) {
        perror("[-] connect");
        close(sock_fd);
        freeaddrinfo(res);
        return 1;
    }
    freeaddrinfo(res);

    signal(SIGWINCH, sigwinch_handler);
    signal(SIGPIPE, SIG_IGN);
    signal(SIGTERM, sigexit_handler);
    signal(SIGQUIT, sigexit_handler);

    /* Enable raw terminal mode so arrows, tab, Ctrl-C are transmitted cleanly */
    enable_raw_mode();

    struct pollfd fds[2];
    fds[0].fd = STDIN_FILENO;
    fds[0].events = POLLIN;
    fds[1].fd = sock_fd;
    fds[1].events = POLLIN;

    char buf[4096];

    while (1) {
        if (g_resized) {
            g_resized = 0;
            if (g_is_attached) {
                send_window_size(sock_fd);
            }
        }

        int ret = poll(fds, 2, 100);
        if (ret < 0) {
            if (errno == EINTR) continue;
            break;
        }

        /* 1. Input from operator keyboard -> send to Relay */
        if (fds[0].revents & POLLIN) {
            ssize_t n = read(STDIN_FILENO, buf, sizeof(buf));
            if (n > 0) {
                /* If in menu mode (not yet attached), translate raw \r to \r\n so readline parses it */
                if (!g_is_attached && n == 1 && buf[0] == '\r') {
                    send(sock_fd, "\r\n", 2, 0);
                } else {
                    send(sock_fd, buf, n, 0);
                }
            } else if (n == 0) {
                break;
            }
        }

        /* 2. Output from Relay -> display on operator screen */
        if (fds[1].revents & POLLIN) {
            ssize_t n = recv(sock_fd, buf, sizeof(buf), 0);
            if (n > 0) {
                write(STDOUT_FILENO, buf, n);

                /* Track attachment status to dynamically send accurate window size */
                if (!g_is_attached && memmem(buf, n, "[+] Attached to [", 17) != NULL) {
                    g_is_attached = 1;
                    /* Immediately sync true terminal cols and rows with remote PTY */
                    send_window_size(sock_fd);
                } else if (g_is_attached && memmem(buf, n, "[*] Detached from", 17) != NULL) {
                    g_is_attached = 0;
                }
            } else {
                break;
            }
        }

        if (fds[1].revents & (POLLHUP | POLLERR)) {
            break;
        }
    }

    restore_terminal();
    close(sock_fd);
    printf("\r\n[*] Disconnected.\r\n");
    return 0;
}
