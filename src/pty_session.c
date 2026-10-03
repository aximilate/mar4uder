#define _GNU_SOURCE
#include "pty_session.h"
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <fcntl.h>
#include <pty.h>
#include <utmp.h>
#include <sys/ioctl.h>
#include <sys/wait.h>
#include <signal.h>
#include <string.h>
#include <errno.h>
#include <termios.h>

int pty_session_init(struct pty_session *sess, const char *shell, uint16_t cols, uint16_t rows) {
    if (!sess) return -1;
    memset(sess, 0, sizeof(*sess));
    sess->master_fd = -1;
    sess->child_pid = -1;

    struct winsize ws;
    memset(&ws, 0, sizeof(ws));
    ws.ws_col = (cols > 0) ? cols : 80;
    ws.ws_row = (rows > 0) ? rows : 24;
    sess->cols = ws.ws_col;
    sess->rows = ws.ws_row;

    char inrc_file[64];
    snprintf(inrc_file, sizeof(inrc_file), "/tmp/.m4r_inrc_%d_%u", (int)getpid(), (unsigned int)rand());
    snprintf(sess->inputrc_path, sizeof(sess->inputrc_path), "%s", inrc_file);

    int inrc_fd = open(inrc_file, O_WRONLY | O_CREAT | O_EXCL, 0600);
    if (inrc_fd >= 0) {
        const char *cfg = "set enable-bracketed-paste off\nset colored-stats on\nset show-all-if-ambiguous on\n";
        ssize_t written = write(inrc_fd, cfg, strlen(cfg));
        (void)written;
        close(inrc_fd);
    }

    pid_t pid = forkpty(&sess->master_fd, NULL, NULL, &ws);
    if (pid < 0) {
        perror("forkpty");
        if (sess->inputrc_path[0]) {
            unlink(sess->inputrc_path);
            sess->inputrc_path[0] = '\0';
        }
        return -1;
    }

    if (pid == 0) {
        /* Child process: inside slave PTY */
        setenv("TERM", "xterm-256color", 1);
        setenv("COLORTERM", "truecolor", 1);
        if (inrc_file[0]) {
            setenv("INPUTRC", inrc_file, 1);
        }

        const char *sh = shell;
        if (!sh || strlen(sh) == 0) {
            sh = getenv("SHELL");
        }
        if (!sh || strlen(sh) == 0) {
            sh = "/bin/bash";
        }

        /* Check if shell exists, fallback to /bin/sh */
        if (access(sh, X_OK) != 0) {
            sh = "/bin/sh";
        }

        /* Start shell session */
        execl(sh, sh, (char *)NULL);
        perror("execl");
        _exit(127);
    }

    /* Parent process */
    sess->child_pid = pid;
    sess->is_running = 1;

    /* Set master FD to non-blocking for event loop multiplexing */
    int flags = fcntl(sess->master_fd, F_GETFL, 0);
    if (flags != -1) {
        fcntl(sess->master_fd, F_SETFL, flags | O_NONBLOCK);
    }

    return 0;
}

int pty_session_is_alive(struct pty_session *sess) {
    if (!sess || sess->child_pid <= 0) return 0;
    int status;
    pid_t res = waitpid(sess->child_pid, &status, WNOHANG);
    if (res == 0) return 1; /* still running */
    return 0;
}

int pty_session_resize(struct pty_session *sess, uint16_t cols, uint16_t rows) {
    if (!sess || sess->master_fd < 0) return -1;

    struct winsize ws;
    memset(&ws, 0, sizeof(ws));
    ws.ws_col = (cols > 0) ? cols : 80;
    ws.ws_row = (rows > 0) ? rows : 24;

    if (ioctl(sess->master_fd, TIOCSWINSZ, &ws) < 0) {
        perror("ioctl(TIOCSWINSZ)");
        return -1;
    }

    sess->cols = ws.ws_col;
    sess->rows = ws.ws_row;
    return 0;
}

ssize_t pty_session_read(struct pty_session *sess, void *buf, size_t count) {
    if (!sess || sess->master_fd < 0) return -1;
    return read(sess->master_fd, buf, count);
}

ssize_t pty_session_write(struct pty_session *sess, const void *buf, size_t count) {
    if (!sess || sess->master_fd < 0) return -1;
    return write(sess->master_fd, buf, count);
}

void pty_session_close(struct pty_session *sess) {
    if (!sess) return;

    if (sess->child_pid > 0) {
        kill(sess->child_pid, SIGHUP);
        int status;
        pid_t res = 0;
        for (int i = 0; i < 10; i++) {
            res = waitpid(sess->child_pid, &status, WNOHANG);
            if (res > 0) break;
            usleep(10000);
        }
        if (res == 0) {
            kill(sess->child_pid, SIGKILL);
            waitpid(sess->child_pid, &status, 0);
        }
        sess->child_pid = -1;
    }

    if (sess->master_fd >= 0) {
        close(sess->master_fd);
        sess->master_fd = -1;
    }

    if (sess->inputrc_path[0]) {
        unlink(sess->inputrc_path);
        sess->inputrc_path[0] = '\0';
    }

    sess->is_running = 0;
}
