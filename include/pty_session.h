#ifndef MAR4UDER_PTY_SESSION_H
#define MAR4UDER_PTY_SESSION_H

#include <stdint.h>
#include <stddef.h>

#include <sys/types.h>

struct pty_session {
    int master_fd;
    pid_t child_pid;
    int is_running;
    uint16_t cols;
    uint16_t rows;
    char inputrc_path[64];
};

/* Spawns shell inside pseudo-terminal */
int pty_session_init(struct pty_session *sess, const char *shell, uint16_t cols, uint16_t rows);

/* Checks if child session process is still running */
int pty_session_is_alive(struct pty_session *sess);

/* Updates terminal size dynamically */
int pty_session_resize(struct pty_session *sess, uint16_t cols, uint16_t rows);

/* Reads terminal output from PTY master */
ssize_t pty_session_read(struct pty_session *sess, void *buf, size_t count);

/* Writes input characters/keys to PTY master */
ssize_t pty_session_write(struct pty_session *sess, const void *buf, size_t count);

/* Closes PTY and cleans up process */
void pty_session_close(struct pty_session *sess);

#endif /* MAR4UDER_PTY_SESSION_H */
