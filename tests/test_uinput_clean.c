#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/ioctl.h>
#include <sys/time.h>
#include <time.h>
#include <stdint.h>

#define EV_SYN 0x00
#define EV_KEY 0x01
#define EV_REL 0x02
#define EV_ABS 0x03
#define SYN_REPORT 0

#define REL_X 0x00
#define REL_Y 0x01
#define BTN_MOUSE 0x110
#define BTN_LEFT 0x110
#define BTN_RIGHT 0x111
#define BTN_MIDDLE 0x112

#define UI_SET_EVBIT   _IOW('U', 100, int)
#define UI_SET_KEYBIT  _IOW('U', 101, int)
#define UI_SET_RELBIT  _IOW('U', 102, int)
#define UI_DEV_CREATE  _IO('U', 1)
#define UI_DEV_DESTROY _IO('U', 2)

struct input_event {
    struct timeval time;
    uint16_t type;
    uint16_t code;
    int32_t value;
};

struct input_id {
    uint16_t bustype;
    uint16_t vendor;
    uint16_t product;
    uint16_t version;
};

struct uinput_user_dev {
    char name[80];
    struct input_id id;
    uint32_t ff_effects_max;
    int32_t absmax[64];
    int32_t absmin[64];
    int32_t absfuzz[64];
    int32_t absflat[64];
};

static void emit(int fd, uint16_t type, uint16_t code, int32_t val) {
    struct input_event ie;
    memset(&ie, 0, sizeof(ie));
    gettimeofday(&ie.time, NULL);
    ie.type = type;
    ie.code = code;
    ie.value = val;
    write(fd, &ie, sizeof(ie));
}

int main(void) {
    int fd = open("/dev/uinput", O_WRONLY | O_NONBLOCK);
    if (fd < 0) {
        perror("open uinput");
        return 1;
    }

    ioctl(fd, UI_SET_EVBIT, EV_KEY);
    ioctl(fd, UI_SET_KEYBIT, BTN_LEFT);
    ioctl(fd, UI_SET_KEYBIT, BTN_RIGHT);
    ioctl(fd, UI_SET_KEYBIT, BTN_MIDDLE);

    ioctl(fd, UI_SET_EVBIT, EV_REL);
    ioctl(fd, UI_SET_RELBIT, REL_X);
    ioctl(fd, UI_SET_RELBIT, REL_Y);

    struct uinput_user_dev uidev;
    memset(&uidev, 0, sizeof(uidev));
    snprintf(uidev.name, sizeof(uidev.name), "mar4uder-virtual-mouse");
    uidev.id.bustype = 0x03; /* BUS_USB */
    uidev.id.vendor  = 0x1234;
    uidev.id.product = 0x5678;
    uidev.id.version = 1;

    if (write(fd, &uidev, sizeof(uidev)) < 0) {
        perror("write uinput_user_dev");
        close(fd);
        return 1;
    }

    if (ioctl(fd, UI_DEV_CREATE) < 0) {
        perror("UI_DEV_CREATE");
        close(fd);
        return 1;
    }

    printf("[+] Virtual mouse created successfully!\n");
    sleep(1);

    /* Move mouse right 50 pixels and down 50 pixels */
    emit(fd, EV_REL, REL_X, 50);
    emit(fd, EV_REL, REL_Y, 50);
    emit(fd, EV_SYN, SYN_REPORT, 0);

    printf("[+] Emitted relative motion\n");
    sleep(1);

    ioctl(fd, UI_DEV_DESTROY);
    close(fd);
    printf("[+] Destroyed virtual mouse\n");
    return 0;
}
