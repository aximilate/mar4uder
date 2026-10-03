#ifndef MAR4UDER_VNC_SERVER_H
#define MAR4UDER_VNC_SERVER_H

#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <pthread.h>

enum vnc_capture_mode {
    VNC_MODE_NONE = 0,
    VNC_MODE_X11,       /* Live native X11 capture via XGetImage / XShm */
    VNC_MODE_DEV_FB,    /* Direct /dev/fb0 */
    VNC_MODE_VIRTUAL    /* Synthetic virtual canvas (headless) */
};

struct vnc_framebuffer {
    uint16_t width;
    uint16_t height;
    uint8_t  bpp;           /* bits per pixel (typically 32) */
    uint32_t stride;        /* bytes per line */
    uint8_t *pixels;        /* front buffer: current displayed frame */
    uint8_t *prev_pixels;   /* shadow buffer for dirty tile diff */
    int is_fb_device;       /* 1 if /dev/fb0 is mapped */
    int capture_mode;       /* enum vnc_capture_mode */
    uint16_t orig_width;
    uint16_t orig_height;
    int scale;
    
    /* Native X11 Dynamic Library handles */
    void *x11_lib;
    void *xtst_lib;
    void *x11_display;
    unsigned long x11_root;
};

/* Initializes screen capture (auto-detects X11, falls back to /dev/fb0 or virtual) */
int vnc_fb_init(struct vnc_framebuffer *fb);

/* Updates framebuffer content with latest screen pixels */
void vnc_fb_update(struct vnc_framebuffer *fb, const char *info_str);

/* Injects mouse/key input into local OS if supported */
void vnc_input_inject(uint8_t event_type, uint8_t button_mask, uint16_t x, uint16_t y, uint32_t key_sym, uint8_t down_flag);

/* Releases framebuffer resources */
void vnc_fb_close(struct vnc_framebuffer *fb);

/* Builds an RFB 3.8 FramebufferUpdate packet for the given region */
size_t vnc_build_rfb_update(struct vnc_framebuffer *fb, uint16_t x, uint16_t y,
                            uint16_t w, uint16_t h, uint8_t *out_buf, size_t max_len);

#endif /* MAR4UDER_VNC_SERVER_H */
