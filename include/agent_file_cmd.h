#ifndef MAR4UDER_AGENT_FILE_CMD_H
#define MAR4UDER_AGENT_FILE_CMD_H

#include "protocol.h"
#include <stdint.h>
#include <stddef.h>

/* Handle incoming file / command messages.
 * Returns 0 if handled, -1 if not handled. */
int handle_file_cmd_packet(int sock_fd, const struct packet_header *hdr, const uint8_t *payload, size_t payload_len);

/* Tick function for active file transfers or running background commands */
void file_cmd_tick(int sock_fd);

/* Cleanup any open files/processes */
void file_cmd_cleanup(void);

#endif /* MAR4UDER_AGENT_FILE_CMD_H */
