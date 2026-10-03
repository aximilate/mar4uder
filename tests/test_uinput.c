#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <linux/uinput.h>

int main(void) {
    int fd = open("/dev/uinput", O_WRONLY | O_NONBLOCK);
    printf("uinput fd: %d\n", fd);
    if (fd >= 0) close(fd);
    return 0;
}
