#define _GNU_SOURCE
#include "pty_session.h"
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <termios.h>
#include <sys/ioctl.h>
#include <signal.h>
#include <poll.h>
#include <errno.h>

static struct termios orig_termios;
static int raw_mode_active = 0;
static struct pty_session g_session;
static volatile sig_atomic_t g_resized = 0;

static void restore_terminal(void) {
    if (raw_mode_active) {
        tcsetattr(STDIN_FILENO, TCSAFLUSH, &orig_termios);
        raw_mode_active = 0;
    }
}

static void enable_raw_mode(void) {
    if (tcgetattr(STDIN_FILENO, &orig_termios) < 0) {
        perror("tcgetattr");
        return;
    }
    atexit(restore_terminal);

    struct termios raw = orig_termios;
    /* Input flags: disable break, CR to NL, parity check, strip 8th bit, flow control */
    raw.c_iflag &= ~(BRKINT | ICRNL | INPCK | ISTRIP | IXON);
    /* Output flags: disable post processing */
    raw.c_oflag &= ~(OPOST);
    /* Control flags: set 8 bits/char */
    raw.c_cflag |= (CS8);
    /* Local flags: disable echo, canonical mode, extended input, signals (Ctrl+C, Ctrl+Z) */
    raw.c_lflag &= ~(ECHO | ICANON | IEXTEN | ISIG);
    /* Read minimum 1 char, timeout 0 */
    raw.c_cc[VMIN] = 1;
    raw.c_cc[VTIME] = 0;

    if (tcsetattr(STDIN_FILENO, TCSAFLUSH, &raw) < 0) {
        perror("tcsetattr");
        return;
    }
    raw_mode_active = 1;
}

static void sigwinch_handler(int sig) {
    (void)sig;
    g_resized = 1;
}

static void update_winsize(void) {
    struct winsize ws;
    if (ioctl(STDIN_FILENO, TIOCGWINSZ, &ws) == 0) {
        pty_session_resize(&g_session, ws.ws_col, ws.ws_row);
    }
}

int main(void) {
    struct winsize ws;
    if (ioctl(STDIN_FILENO, TIOCGWINSZ, &ws) < 0) {
        ws.ws_col = 80;
        ws.ws_row = 24;
    }

    if (pty_session_init(&g_session, NULL, ws.ws_col, ws.ws_row) < 0) {
        fprintf(stderr, "Failed to initialize PTY session\n");
        return 1;
    }

    signal(SIGWINCH, sigwinch_handler);
    enable_raw_mode();

    struct pollfd fds[2];
    fds[0].fd = STDIN_FILENO;
    fds[0].events = POLLIN;
    fds[1].fd = g_session.master_fd;
    fds[1].events = POLLIN;

    char buf[4096];

    while (g_session.is_running) {
        if (g_resized) {
            g_resized = 0;
            update_winsize();
        }

        int ret = poll(fds, 2, 100);
        if (ret < 0) {
            if (errno == EINTR) continue;
            break;
        }

        /* Input from user -> write to PTY */
        if (fds[0].revents & POLLIN) {
            ssize_t n = read(STDIN_FILENO, buf, sizeof(buf));
            if (n > 0) {
                pty_session_write(&g_session, buf, n);
            } else if (n == 0) {
                break;
            }
        }

        /* Output from PTY -> write to user stdout */
        if (fds[1].revents & POLLIN) {
            ssize_t n = pty_session_read(&g_session, buf, sizeof(buf));
            if (n > 0) {
                write(STDOUT_FILENO, buf, n);
            } else if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
                /* Non-blocking read would block, ignore */
            } else {
                /* Shell exited or EOF */
                break;
            }
        }

        if (fds[1].revents & (POLLHUP | POLLERR)) {
            break;
        }
    }

    restore_terminal();
    pty_session_close(&g_session);
    return 0;
}
