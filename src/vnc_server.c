#define _GNU_SOURCE
#include "vnc_server.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <errno.h>
#include <dirent.h>
#include <signal.h>
#include <dlfcn.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/types.h>
#include <time.h>

#define DEFAULT_VIRTUAL_W 1024
#define DEFAULT_VIRTUAL_H 768

#ifndef FBIOGET_VSCREENINFO
#define FBIOGET_VSCREENINFO 0x4600

struct fb_var_screeninfo {
    uint32_t xres;
    uint32_t yres;
    uint32_t xres_virtual;
    uint32_t yres_virtual;
    uint32_t xoffset;
    uint32_t yoffset;
    uint32_t bits_per_pixel;
    uint32_t grayscale;
    uint32_t red_offset, red_length, red_msb_right;
    uint32_t green_offset, green_length, green_msb_right;
    uint32_t blue_offset, blue_length, blue_msb_right;
    uint32_t transp_offset, transp_length, transp_msb_right;
    uint32_t nonstd, activate, height, width, accel_flags, pixclock;
    uint32_t left_margin, right_margin, upper_margin, lower_margin;
    uint32_t hsync_len, vsync_len, sync, vmode, rotate, colorspace;
    uint32_t reserved[4];
};
#endif

/* X11 Minimal C types & function pointers */
typedef void* DisplayPtr;
typedef unsigned long Window;

typedef struct {
    int x, y;
    int width, height;
    int border_width;
    int depth;
    void *visual;
    Window root;
    int class;
    int bit_gravity;
    int win_gravity;
    int backing_store;
    unsigned long backing_planes;
    unsigned long backing_pixel;
    int save_under;
    void *colormap;
    int map_installed;
    int map_state;
    long all_event_masks;
    long your_event_mask;
    long do_not_propagate_mask;
    int override_redirect;
    void *screen;
} XWindowAttributes_t;

typedef struct {
    int width, height;
    int xoffset;
    int format;
    char *data;
    int byte_order;
    int bitmap_unit;
    int bitmap_bit_order;
    int bitmap_pad;
    int depth;
    int bytes_per_line;
    int bits_per_pixel;
    unsigned long red_mask;
    unsigned long green_mask;
    unsigned long blue_mask;
    char *obdata;
    struct {
        void *(*create_image)();
        int (*destroy_image)(void*);
    } f;
} XImage_t;

static DisplayPtr (*fn_XOpenDisplay)(const char*) = NULL;
static int (*fn_XCloseDisplay)(DisplayPtr) = NULL;
static Window (*fn_XDefaultRootWindow)(DisplayPtr) = NULL;
static int (*fn_XGetWindowAttributes)(DisplayPtr, Window, XWindowAttributes_t*) = NULL;
static XImage_t* (*fn_XGetImage)(DisplayPtr, Window, int, int, unsigned int, unsigned int, unsigned long, int) = NULL;
static int (*fn_XDestroyImage)(XImage_t*) = NULL;
static int (*fn_XFlush)(DisplayPtr) = NULL;
static unsigned char (*fn_XKeysymToKeycode)(DisplayPtr, unsigned long) = NULL;
static int (*fn_XWarpPointer)(DisplayPtr, Window, Window, int, int, unsigned int, unsigned int, int, int) = NULL;
static int (*fn_XDefaultScreen)(DisplayPtr) = NULL;
static int (*fn_XDisplayWidth)(DisplayPtr, int) = NULL;
static int (*fn_XDisplayHeight)(DisplayPtr, int) = NULL;

/* XTest function pointers */
static int (*fn_XTestFakeMotionEvent)(DisplayPtr, int, int, int, unsigned long) = NULL;
static int (*fn_XTestFakeButtonEvent)(DisplayPtr, unsigned int, int, unsigned long) = NULL;
static int (*fn_XTestFakeKeyEvent)(DisplayPtr, unsigned int, int, unsigned long) = NULL;

static int g_fb_fd = -1;
static size_t g_fb_size = 0;
static void *g_fb_mmap = NULL;
static int g_uinput_fd = -1;
static struct vnc_framebuffer *g_current_fb = NULL;

/* Draw 8x8 font characters on virtual canvas */
static void draw_char(uint32_t *buf, int stride_pixels, int x, int y, char c, uint32_t color) {
    for (int r = 0; r < 8; r++) {
        for (int col = 0; col < 8; col++) {
            if ((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ':' || c == '.' || c == '-' || c == ' ') {
                if (r == 0 || r == 7 || col == 0 || col == 7 || (r == col)) {
                    buf[(y + r) * stride_pixels + (x + col)] = color;
                }
            }
        }
    }
}

static DisplayPtr g_input_x11_disp = NULL;
static Window     g_input_x11_root = 0;
static void      *g_input_x11_lib  = NULL;
static void      *g_input_xtst_lib = NULL;

static void find_xauth_from_proc(char *out_display, size_t disp_len, char *out_xauth, size_t auth_len) {
    DIR *pd = opendir("/proc");
    if (!pd) return;
    struct dirent *de;
    while ((de = readdir(pd)) != NULL) {
        if (de->d_name[0] < '0' || de->d_name[0] > '9') continue;

        /* 1. Try reading environ of active desktop session */
        if (out_xauth[0] == '\0' || out_display[0] == '\0') {
            char epath[128];
            snprintf(epath, sizeof(epath), "/proc/%s/environ", de->d_name);
            int efd = open(epath, O_RDONLY);
            if (efd >= 0) {
                char ebuf[4096];
                ssize_t en = read(efd, ebuf, sizeof(ebuf) - 1);
                close(efd);
                if (en > 0) {
                    ebuf[en] = '\0';
                    ssize_t pos = 0;
                    while (pos < en) {
                        const char *entry = ebuf + pos;
                        size_t elen = strlen(entry);
                        if (strncmp(entry, "DISPLAY=", 8) == 0 && out_display[0] == '\0') {
                            snprintf(out_display, disp_len, "%s", entry + 8);
                        } else if (strncmp(entry, "XAUTHORITY=", 11) == 0 && out_xauth[0] == '\0') {
                            const char *cand = entry + 11;
                            if (strstr(cand, "iceauth") == NULL && access(cand, R_OK) == 0) {
                                snprintf(out_xauth, auth_len, "%s", cand);
                            }
                        }
                        pos += elen + 1;
                    }
                }
            }
        }

        /* 2. Try reading -auth from running Xorg/X cmdline */
        if (out_xauth[0] == '\0') {
            char cpath[128];
            snprintf(cpath, sizeof(cpath), "/proc/%s/cmdline", de->d_name);
            int fd = open(cpath, O_RDONLY);
            if (fd >= 0) {
                char buf[1024];
                ssize_t n = read(fd, buf, sizeof(buf) - 1);
                close(fd);
                if (n > 0) {
                    buf[n] = '\0';
                    for (ssize_t i = 0; i < n - 6; i++) {
                        if (strcmp(buf + i, "-auth") == 0) {
                            size_t path_offset = i + strlen("-auth") + 1;
                            if (path_offset < (size_t)n) {
                                const char *cand = buf + path_offset;
                                if (strstr(cand, "iceauth") == NULL && access(cand, R_OK) == 0) {
                                    snprintf(out_xauth, auth_len, "%s", cand);
                                    break;
                                }
                            }
                        }
                    }
                }
            }
        }

        if (out_display[0] != '\0' && out_xauth[0] != '\0') {
            break;
        }
    }
    closedir(pd);
}

/* Locate active X11 display and authority */
static int find_x11_session(char *out_display, size_t disp_len, char *out_xauth, size_t auth_len) {
    out_display[0] = '\0';
    out_xauth[0] = '\0';

    const char *env_disp = getenv("DISPLAY");
    if (env_disp && strlen(env_disp) > 0) {
        snprintf(out_display, disp_len, "%s", env_disp);
    }

    const char *env_auth = getenv("XAUTHORITY");
    if (env_auth && strstr(env_auth, "iceauth") == NULL && access(env_auth, R_OK) == 0) {
        snprintf(out_xauth, auth_len, "%s", env_auth);
    }

    /* 1. Try reading from running /proc/ environ & cmdline */
    find_xauth_from_proc(out_display, disp_len, out_xauth, auth_len);

    if (out_display[0] == '\0') {
        if (access("/tmp/.X11-unix/X0", F_OK) == 0) {
            snprintf(out_display, disp_len, ":0");
        } else {
            DIR *d = opendir("/tmp/.X11-unix");
            if (d) {
                struct dirent *de;
                while ((de = readdir(d)) != NULL) {
                    if (de->d_name[0] == 'X' && de->d_name[1] >= '0' && de->d_name[1] <= '9') {
                        snprintf(out_display, disp_len, ":%s", de->d_name + 1);
                        break;
                    }
                }
                closedir(d);
            }
        }
    }
    if (out_display[0] == '\0') {
        snprintf(out_display, disp_len, ":0");
    }

    if (out_xauth[0] != '\0') {
        return 0;
    }

    /* 2. Common Xauthority locations (Strictly ignore iceauth) */
    const char *search_dirs[] = {"/tmp", "/run/sddm", "/var/run/sddm", "/run/user/500", "/run/user/1000", "/run/user", NULL};
    for (int i = 0; search_dirs[i]; i++) {
        DIR *d = opendir(search_dirs[i]);
        if (d) {
            struct dirent *de;
            while ((de = readdir(d)) != NULL) {
                if (strstr(de->d_name, "iceauth") != NULL) continue;
                if (strstr(de->d_name, "xauth") != NULL ||
                    strstr(de->d_name, "Xauthority") != NULL ||
                    strstr(de->d_name, "serverauth") != NULL) {
                    char cand[256];
                    snprintf(cand, sizeof(cand), "%s/%s", search_dirs[i], de->d_name);
                    if (access(cand, R_OK) == 0) {
                        snprintf(out_xauth, auth_len, "%s", cand);
                        closedir(d);
                        return 0;
                    }
                }
            }
            closedir(d);
        }
    }

    if (access("/root/.Xauthority", R_OK) == 0) {
        snprintf(out_xauth, auth_len, "/root/.Xauthority");
        return 0;
    }

    DIR *hd = opendir("/home");
    if (hd) {
        struct dirent *de;
        while ((de = readdir(hd)) != NULL) {
            if (de->d_name[0] != '.') {
                char test_path[256];
                snprintf(test_path, sizeof(test_path), "/home/%s/.Xauthority", de->d_name);
                if (access(test_path, R_OK) == 0) {
                    snprintf(out_xauth, auth_len, "%s", test_path);
                    closedir(hd);
                    return 0;
                }
            }
        }
        closedir(hd);
    }
    return 0;
}

static int load_x11_symbols(struct vnc_framebuffer *fb) {
    const char *x11_paths[] = {
        "libX11.so.6",
        "/usr/lib/x86_64-linux-gnu/libX11.so.6",
        "/usr/lib64/libX11.so.6",
        "/usr/lib/libX11.so.6",
        NULL
    };

    void *lib = NULL;
    for (int i = 0; x11_paths[i]; i++) {
        lib = dlopen(x11_paths[i], RTLD_LAZY | RTLD_GLOBAL);
        if (lib) break;
    }
    if (!lib) return -1;

    fn_XOpenDisplay = dlsym(lib, "XOpenDisplay");
    fn_XCloseDisplay = dlsym(lib, "XCloseDisplay");
    fn_XDefaultRootWindow = dlsym(lib, "XDefaultRootWindow");
    fn_XGetWindowAttributes = dlsym(lib, "XGetWindowAttributes");
    fn_XGetImage = dlsym(lib, "XGetImage");
    fn_XDestroyImage = dlsym(lib, "XDestroyImage");
    fn_XFlush = dlsym(lib, "XFlush");
    fn_XKeysymToKeycode = dlsym(lib, "XKeysymToKeycode");
    fn_XWarpPointer = dlsym(lib, "XWarpPointer");

    if (!fn_XOpenDisplay || !fn_XDefaultRootWindow || !fn_XGetWindowAttributes || !fn_XGetImage) {
        dlclose(lib);
        return -1;
    }

    fb->x11_lib = lib;

    /* Load XTest for native input */
    const char *xtst_paths[] = {
        "libXtst.so.6",
        "/usr/lib/x86_64-linux-gnu/libXtst.so.6",
        "/usr/lib64/libXtst.so.6",
        "/usr/lib/libXtst.so.6",
        NULL
    };
    void *tlib = NULL;
    for (int i = 0; xtst_paths[i]; i++) {
        tlib = dlopen(xtst_paths[i], RTLD_LAZY);
        if (tlib) break;
    }
    if (tlib) {
        fn_XTestFakeMotionEvent = dlsym(tlib, "XTestFakeMotionEvent");
        fn_XTestFakeButtonEvent = dlsym(tlib, "XTestFakeButtonEvent");
        fn_XTestFakeKeyEvent = dlsym(tlib, "XTestFakeKeyEvent");
        fb->xtst_lib = tlib;
    }

    return 0;
}

static void init_uinput_fallback(void) {
    if (g_uinput_fd >= 0) return;
    g_uinput_fd = open("/dev/uinput", O_WRONLY | O_NONBLOCK);
    if (g_uinput_fd < 0) g_uinput_fd = open("/dev/input/uinput", O_WRONLY | O_NONBLOCK);
    if (g_uinput_fd >= 0) {
        ioctl(g_uinput_fd, 0x40045564 /* UI_SET_EVBIT */, 0x01 /* EV_KEY */);
        ioctl(g_uinput_fd, 0x40045565 /* UI_SET_KEYBIT */, 0x110 /* BTN_LEFT */);
        ioctl(g_uinput_fd, 0x40045565 /* UI_SET_KEYBIT */, 0x111 /* BTN_RIGHT */);
        ioctl(g_uinput_fd, 0x40045565 /* UI_SET_KEYBIT */, 0x112 /* BTN_MIDDLE */);
        for (int k = 1; k < 256; k++) {
            ioctl(g_uinput_fd, 0x40045565 /* UI_SET_KEYBIT */, k);
        }
        ioctl(g_uinput_fd, 0x40045564 /* UI_SET_EVBIT */, 0x02 /* EV_REL */);
        ioctl(g_uinput_fd, 0x40045566 /* UI_SET_RELBIT */, 0x00 /* REL_X */);
        ioctl(g_uinput_fd, 0x40045566 /* UI_SET_RELBIT */, 0x01 /* REL_Y */);

        char dev_setup[1024];
        memset(dev_setup, 0, sizeof(dev_setup));
        snprintf(dev_setup, 80, "mar4uder-uinput");
        ssize_t wres = write(g_uinput_fd, dev_setup, sizeof(dev_setup));
        (void)wres;
        ioctl(g_uinput_fd, 0x5501 /* UI_DEV_CREATE */);
    }
}

int vnc_fb_init(struct vnc_framebuffer *fb) {
    if (!fb) return -1;
    memset(fb, 0, sizeof(*fb));
    g_current_fb = fb;

    /* 1. Try Native Direct X11 Capture (Zero ffmpeg, zero python) */
    char display[64];
    char xauth[256];
    if (find_x11_session(display, sizeof(display), xauth, sizeof(xauth)) == 0) {
        if (strlen(xauth) > 0) setenv("XAUTHORITY", xauth, 1);
        if (strlen(display) > 0) setenv("DISPLAY", display, 1);

        if (load_x11_symbols(fb) == 0) {
            DisplayPtr disp = fn_XOpenDisplay(display);
            if (!disp) disp = fn_XOpenDisplay(NULL);

            if (disp) {
                Window root = fn_XDefaultRootWindow(disp);
                XWindowAttributes_t attr;
                memset(&attr, 0, sizeof(attr));
                if (fn_XGetWindowAttributes(disp, root, &attr) != 0 && attr.width > 0 && attr.height > 0) {
                    uint16_t ow = (uint16_t)attr.width;
                    uint16_t oh = (uint16_t)attr.height;
                    fb->orig_width = ow;
                    fb->orig_height = oh;

                    /* Downscale to 720p / HD to maximize responsiveness and minimize network bandwidth */
                    if (ow >= 3840 || oh >= 2160) {
                        fb->scale = 3;
                        fb->width = ow / 3;
                        fb->height = oh / 3;
                        printf("[+] VNC: Detected 4K screen (%dx%d), downscaling 3x to 720p (%dx%d)\n",
                               ow, oh, fb->width, fb->height);
                    } else if (ow > 1280 || oh > 720) {
                        fb->scale = 2;
                        fb->width = ow / 2;
                        fb->height = oh / 2;
                        printf("[+] VNC: Detected HD screen (%dx%d), downscaling 2x to (%dx%d)\n",
                               ow, oh, fb->width, fb->height);
                    } else {
                        fb->scale = 1;
                        fb->width = ow;
                        fb->height = oh;
                    }
                    fb->bpp = 32;
                    fb->stride = fb->width * 4;

                    size_t frame_bytes = (size_t)fb->width * fb->height * 4;
                    fb->pixels = (uint8_t *)calloc(1, frame_bytes);
                    fb->prev_pixels = (uint8_t *)calloc(1, frame_bytes);

                    if (fb->pixels && fb->prev_pixels) {
                        memset(fb->prev_pixels, 0xFF, frame_bytes);
                        fb->x11_display = disp;
                        fb->x11_root = root;
                        fb->capture_mode = VNC_MODE_X11;
                        printf("[+] VNC: Native X11 attached (%dx%d, auth: %s, direct XGetImage)\n",
                               fb->width, fb->height, strlen(xauth) > 0 ? xauth : "none");
                        return 0;
                    }
                }
                fn_XCloseDisplay(disp);
            }
        }
    }

    /* 2. Try Direct Linux Framebuffer (/dev/fb0) */
    g_fb_fd = open("/dev/fb0", O_RDONLY);
    if (g_fb_fd >= 0) {
        struct fb_var_screeninfo vinfo;
        if (ioctl(g_fb_fd, FBIOGET_VSCREENINFO, &vinfo) == 0 && vinfo.xres > 0 && vinfo.yres > 0) {
            fb->width = vinfo.xres;
            fb->height = vinfo.yres;
            fb->bpp = (vinfo.bits_per_pixel > 0) ? vinfo.bits_per_pixel : 32;
            fb->stride = fb->width * (fb->bpp / 8);
            g_fb_size = (size_t)vinfo.yres_virtual * vinfo.xres_virtual * (fb->bpp / 8);

            g_fb_mmap = mmap(NULL, g_fb_size, PROT_READ, MAP_SHARED, g_fb_fd, 0);
            if (g_fb_mmap != MAP_FAILED) {
                fb->pixels = (uint8_t *)calloc(1, g_fb_size);
                fb->prev_pixels = (uint8_t *)calloc(1, g_fb_size);
                if (fb->pixels && fb->prev_pixels) {
                    memcpy(fb->pixels, g_fb_mmap, g_fb_size);
                    memset(fb->prev_pixels, 0xFF, g_fb_size);
                    fb->is_fb_device = 1;
                    fb->capture_mode = VNC_MODE_DEV_FB;
                    init_uinput_fallback();
                    printf("[+] VNC: Attached to /dev/fb0 (%dx%d, %d bpp)\n",
                           fb->width, fb->height, fb->bpp);
                    return 0;
                }
            }
        }
        close(g_fb_fd);
        g_fb_fd = -1;
    }

    /* 3. Fallback to Virtual Canvas (Headless / Embedded) */
    fb->width = DEFAULT_VIRTUAL_W;
    fb->height = DEFAULT_VIRTUAL_H;
    fb->bpp = 32;
    fb->stride = fb->width * 4;
    size_t sz = (size_t)fb->stride * fb->height;
    fb->pixels = (uint8_t *)calloc(1, sz);
    fb->prev_pixels = (uint8_t *)calloc(1, sz);
    if (fb->prev_pixels) memset(fb->prev_pixels, 0xFF, sz);
    fb->capture_mode = VNC_MODE_VIRTUAL;
    init_uinput_fallback();

    printf("[+] VNC: Initialized Virtual Console Framebuffer (%dx%d, 32 bpp)\n",
           fb->width, fb->height);
    return 0;
}

void vnc_fb_update(struct vnc_framebuffer *fb, const char *info_str) {
    if (!fb || !fb->pixels) return;

    if (fb->capture_mode == VNC_MODE_X11 && fb->x11_display && fn_XGetImage) {
        /* Direct X11 screen capture via XGetImage into pixels */
        uint16_t grab_w = (fb->scale > 1) ? fb->orig_width : fb->width;
        uint16_t grab_h = (fb->scale > 1) ? fb->orig_height : fb->height;
        XImage_t *img = fn_XGetImage(fb->x11_display, fb->x11_root, 0, 0, grab_w, grab_h, ~0UL, 2 /* ZPixmap */);
        if (img && img->data) {
            if (fb->scale > 1) {
                /* Downscale screen using native scale factor */
                uint32_t *src = (uint32_t *)img->data;
                uint32_t *dst = (uint32_t *)fb->pixels;
                int ow = fb->orig_width;
                int dw = fb->width;
                int dh = fb->height;
                int sc = fb->scale;
                for (int y = 0; y < dh; y++) {
                    int sRow = (y * sc) * ow;
                    int dRow = y * dw;
                    for (int x = 0; x < dw; x++) {
                        dst[dRow + x] = src[sRow + (x * sc)];
                    }
                }
            } else {
                size_t frame_bytes = (size_t)fb->width * fb->height * 4;
                memcpy(fb->pixels, img->data, frame_bytes);
            }
            if (img->f.destroy_image) {
                img->f.destroy_image(img);
            }
            return;
        }
        if (img && img->f.destroy_image) {
            img->f.destroy_image(img);
        }
    }

    if (fb->capture_mode == VNC_MODE_DEV_FB && g_fb_mmap && g_fb_mmap != MAP_FAILED) {
        memcpy(fb->pixels, g_fb_mmap, (size_t)fb->stride * fb->height);
        return;
    }

    if (fb->capture_mode == VNC_MODE_VIRTUAL) {
        uint32_t *p = (uint32_t *)fb->pixels;
        int total = fb->width * fb->height;

        for (int i = 0; i < total; i++) {
            int y = i / fb->width;
            uint8_t b = (uint8_t)(15 + (y * 20 / fb->height));
            p[i] = (0xFF << 24) | (10 << 16) | (15 << 8) | b;
        }

        int top_h = (fb->height > 60) ? 40 : (fb->height / 2);
        for (int y = 0; y < top_h; y++) {
            for (int x = 0; x < fb->width; x++) {
                p[y * fb->width + x] = 0xFF0D1B1E;
            }
        }

        time_t now = time(NULL);
        char time_buf[64];
        strftime(time_buf, sizeof(time_buf), "%Y-%m-%d %H:%M:%S", localtime(&now));

        char banner[256];
        snprintf(banner, sizeof(banner), "MAR4UDER CONSOLE: %s | %s",
                 (info_str ? info_str : "ONLINE"), time_buf);

        int start_x = 20;
        int start_y = (top_h > 20) ? 16 : 4;
        for (size_t i = 0; i < strlen(banner); i++) {
            draw_char(p, fb->width, start_x + (int)i * 10, start_y, banner[i], 0xFF00FF66);
        }
    }
}

static DisplayPtr ensure_input_x11_display(void) {
    if (g_input_x11_disp) {
        return g_input_x11_disp;
    }

    if (g_current_fb && g_current_fb->capture_mode == VNC_MODE_X11 && g_current_fb->x11_display) {
        return g_current_fb->x11_display;
    }

    char display[64];
    char xauth[256];
    if (find_x11_session(display, sizeof(display), xauth, sizeof(xauth)) == 0) {
        if (strlen(xauth) > 0) setenv("XAUTHORITY", xauth, 1);
        if (strlen(display) > 0) setenv("DISPLAY", display, 1);
    }

    /* Load X11 symbols if not already loaded */
    if (!fn_XOpenDisplay) {
        const char *x11_paths[] = {
            "libX11.so.6",
            "/usr/lib64/libX11.so.6",
            "/usr/lib/x86_64-linux-gnu/libX11.so.6",
            "/usr/lib/libX11.so.6",
            NULL
        };
        for (int i = 0; x11_paths[i]; i++) {
            g_input_x11_lib = dlopen(x11_paths[i], RTLD_LAZY | RTLD_GLOBAL);
            if (g_input_x11_lib) break;
        }
        if (!g_input_x11_lib) return NULL;

        fn_XOpenDisplay = dlsym(g_input_x11_lib, "XOpenDisplay");
        fn_XCloseDisplay = dlsym(g_input_x11_lib, "XCloseDisplay");
        fn_XDefaultRootWindow = dlsym(g_input_x11_lib, "XDefaultRootWindow");
        fn_XGetWindowAttributes = dlsym(g_input_x11_lib, "XGetWindowAttributes");
        fn_XGetImage = dlsym(g_input_x11_lib, "XGetImage");
        fn_XDestroyImage = dlsym(g_input_x11_lib, "XDestroyImage");
        fn_XFlush = dlsym(g_input_x11_lib, "XFlush");
        fn_XKeysymToKeycode = dlsym(g_input_x11_lib, "XKeysymToKeycode");
        fn_XWarpPointer = dlsym(g_input_x11_lib, "XWarpPointer");
        fn_XDefaultScreen = dlsym(g_input_x11_lib, "XDefaultScreen");
        fn_XDisplayWidth = dlsym(g_input_x11_lib, "XDisplayWidth");
        fn_XDisplayHeight = dlsym(g_input_x11_lib, "XDisplayHeight");
    }

    /* Load Xtst symbols if not already loaded */
    if (!fn_XTestFakeMotionEvent) {
        const char *xtst_paths[] = {
            "libXtst.so.6",
            "/usr/lib64/libXtst.so.6",
            "/usr/lib/x86_64-linux-gnu/libXtst.so.6",
            "/usr/lib/libXtst.so.6",
            NULL
        };
        for (int i = 0; xtst_paths[i]; i++) {
            g_input_xtst_lib = dlopen(xtst_paths[i], RTLD_LAZY);
            if (g_input_xtst_lib) break;
        }
        if (g_input_xtst_lib) {
            fn_XTestFakeMotionEvent = dlsym(g_input_xtst_lib, "XTestFakeMotionEvent");
            fn_XTestFakeButtonEvent = dlsym(g_input_xtst_lib, "XTestFakeButtonEvent");
            fn_XTestFakeKeyEvent = dlsym(g_input_xtst_lib, "XTestFakeKeyEvent");
        }
    }

    if (fn_XOpenDisplay) {
        const char *d = getenv("DISPLAY");
        g_input_x11_disp = fn_XOpenDisplay(d && d[0] ? d : ":0");
        if (!g_input_x11_disp) {
            g_input_x11_disp = fn_XOpenDisplay(NULL);
        }
        if (g_input_x11_disp) {
            if (fn_XDefaultRootWindow) {
                g_input_x11_root = fn_XDefaultRootWindow(g_input_x11_disp);
            }
            printf("[+] X11 Input: successfully connected to X server (disp=%p, DISPLAY=%s, XAUTHORITY=%s)\n",
                   (void *)g_input_x11_disp, getenv("DISPLAY") ? getenv("DISPLAY") : ":0",
                   getenv("XAUTHORITY") ? getenv("XAUTHORITY") : "(none)");
        } else {
            fprintf(stderr, "[-] X11 Input: XOpenDisplay failed (DISPLAY=%s, XAUTHORITY=%s)\n",
                    getenv("DISPLAY") ? getenv("DISPLAY") : ":0",
                    getenv("XAUTHORITY") ? getenv("XAUTHORITY") : "(none)");
        }
    }

    return g_input_x11_disp;
}

void vnc_input_inject(uint8_t event_type, uint8_t button_mask, uint16_t x, uint16_t y, uint32_t key_sym, uint8_t down_flag) {
    static uint8_t last_btn = 0;

    DisplayPtr disp = ensure_input_x11_display();
    if (disp) {
        Window root = g_input_x11_root ? g_input_x11_root : (g_current_fb ? g_current_fb->x11_root : 0);
        if (!root && fn_XDefaultRootWindow) {
            root = fn_XDefaultRootWindow(disp);
        }

        if (event_type == 0) { /* Pointer / Mouse */
            int screen_w = 1920;
            int screen_h = 1080;

            if (fn_XGetWindowAttributes && root) {
                XWindowAttributes_t attr;
                memset(&attr, 0, sizeof(attr));
                if (fn_XGetWindowAttributes(disp, root, &attr) != 0 && attr.width > 0 && attr.height > 0) {
                    screen_w = attr.width;
                    screen_h = attr.height;
                }
            } else if (fn_XDefaultScreen && fn_XDisplayWidth && fn_XDisplayHeight) {
                int scr = fn_XDefaultScreen(disp);
                int dw = fn_XDisplayWidth(disp, scr);
                int dh = fn_XDisplayHeight(disp, scr);
                if (dw > 0 && dh > 0) {
                    screen_w = dw;
                    screen_h = dh;
                }
            }

            /* Map normalized 0..65535 coordinate directly to native physical screen pixels */
            int real_x = (int)(((uint64_t)x * (uint64_t)(screen_w - 1)) / 65535ULL);
            int real_y = (int)(((uint64_t)y * (uint64_t)(screen_h - 1)) / 65535ULL);
            if (real_x < 0) real_x = 0;
            if (real_x >= screen_w) real_x = screen_w - 1;
            if (real_y < 0) real_y = 0;
            if (real_y >= screen_h) real_y = screen_h - 1;

            printf("[DEBUG_INP] in_x=%u in_y=%u screen=%dx%d -> real=(%d,%d)\n",
                   (unsigned int)x, (unsigned int)y, screen_w, screen_h, real_x, real_y);

            if (fn_XWarpPointer && root) {
                fn_XWarpPointer(disp, 0, root, 0, 0, 0, 0, real_x, real_y);
            }
            if (fn_XTestFakeMotionEvent) {
                fn_XTestFakeMotionEvent(disp, -1, real_x, real_y, 0);
            }

            if (fn_XTestFakeButtonEvent) {
                for (int b = 1; b <= 5; b++) {
                    int was_down = (last_btn & (1 << (b - 1))) ? 1 : 0;
                    int is_down = (button_mask & (1 << (b - 1))) ? 1 : 0;
                    if (b == 4 || b == 5) {
                        /* Scroll wheels are pulsed on down */
                        if (is_down) {
                            fn_XTestFakeButtonEvent(disp, b, 1, 0);
                            fn_XTestFakeButtonEvent(disp, b, 0, 0);
                        }
                    } else if (was_down != is_down) {
                        fn_XTestFakeButtonEvent(disp, b, is_down, 0);
                    }
                }
                last_btn = button_mask;
            }
            if (fn_XFlush) fn_XFlush(disp);
            return;
        } else if (event_type == 1) { /* Key */
            if (fn_XKeysymToKeycode && fn_XTestFakeKeyEvent) {
                unsigned char kc = fn_XKeysymToKeycode(disp, key_sym);
                if (kc == 0) {
                    /* Common fallback key mappings */
                    switch (key_sym) {
                        case 13: kc = fn_XKeysymToKeycode(disp, 0xFF0D); break; /* Enter */
                        case 8:  kc = fn_XKeysymToKeycode(disp, 0xFF08); break; /* Backspace */
                        case 9:  kc = fn_XKeysymToKeycode(disp, 0xFF09); break; /* Tab */
                        case 27: kc = fn_XKeysymToKeycode(disp, 0xFF1B); break; /* Esc */
                        case 32: kc = fn_XKeysymToKeycode(disp, 0x0020); break; /* Space */
                    }
                }
                if (kc > 0) {
                    fn_XTestFakeKeyEvent(disp, kc, down_flag ? 1 : 0, 0);
                    if (fn_XFlush) fn_XFlush(disp);
                }
            }
            return;
        }
    }

    /* 2. Direct /dev/uinput fallback */
    if (g_uinput_fd < 0) {
        init_uinput_fallback();
    }
    if (g_uinput_fd >= 0) {
        static uint16_t last_ux = 0, last_uy = 0;
        static int has_upos = 0;

        if (event_type == 0) { /* Pointer / Mouse */
            if (has_upos) {
                int dx = (int)x - (int)last_ux;
                int dy = (int)y - (int)last_uy;
                if (dx != 0 || dy != 0) {
                    struct { struct timeval t; uint16_t type, code; int32_t val; } ev[3];
                    memset(ev, 0, sizeof(ev));
                    ev[0].type = 0x02; ev[0].code = 0x00; ev[0].val = dx;
                    ev[1].type = 0x02; ev[1].code = 0x01; ev[1].val = dy;
                    ev[2].type = 0x00; ev[2].code = 0x00; ev[2].val = 0;
                    ssize_t ew = write(g_uinput_fd, ev, sizeof(ev));
                    (void)ew;
                }
            }
            last_ux = x;
            last_uy = y;
            has_upos = 1;

            if (button_mask != last_btn) {
                struct { struct timeval t; uint16_t type, code; int32_t val; } bev[2];
                memset(bev, 0, sizeof(bev));
                if ((button_mask & 1) != (last_btn & 1)) {
                    bev[0].type = 0x01; bev[0].code = 0x110; bev[0].val = (button_mask & 1) ? 1 : 0;
                    ssize_t ew = write(g_uinput_fd, bev, sizeof(bev)); (void)ew;
                }
                if ((button_mask & 2) != (last_btn & 2)) {
                    bev[0].type = 0x01; bev[0].code = 0x112; bev[0].val = (button_mask & 2) ? 1 : 0;
                    ssize_t ew = write(g_uinput_fd, bev, sizeof(bev)); (void)ew;
                }
                if ((button_mask & 4) != (last_btn & 4)) {
                    bev[0].type = 0x01; bev[0].code = 0x111; bev[0].val = (button_mask & 4) ? 1 : 0;
                    ssize_t ew = write(g_uinput_fd, bev, sizeof(bev)); (void)ew;
                }
                last_btn = button_mask;
            }
        }
    }
}

void vnc_fb_close(struct vnc_framebuffer *fb) {
    if (!fb) return;
    g_current_fb = NULL;

    if (fb->x11_display && fn_XCloseDisplay) {
        fn_XCloseDisplay(fb->x11_display);
        fb->x11_display = NULL;
    }
    if (fb->xtst_lib) {
        dlclose(fb->xtst_lib);
        fb->xtst_lib = NULL;
    }
    if (fb->x11_lib) {
        dlclose(fb->x11_lib);
        fb->x11_lib = NULL;
    }

    if (g_uinput_fd >= 0) {
        ioctl(g_uinput_fd, 0x5502 /* UI_DEV_DESTROY */);
        close(g_uinput_fd);
        g_uinput_fd = -1;
    }

    if (fb->prev_pixels) {
        free(fb->prev_pixels);
        fb->prev_pixels = NULL;
    }
    if (fb->pixels) {
        free(fb->pixels);
        fb->pixels = NULL;
    }

    if (g_fb_mmap && g_fb_mmap != MAP_FAILED) {
        munmap(g_fb_mmap, g_fb_size);
        g_fb_mmap = NULL;
    }
    if (g_fb_fd >= 0) {
        close(g_fb_fd);
        g_fb_fd = -1;
    }

    fb->capture_mode = VNC_MODE_NONE;
}

size_t vnc_build_rfb_update(struct vnc_framebuffer *fb, uint16_t x, uint16_t y,
                            uint16_t w, uint16_t h, uint8_t *out_buf, size_t max_len) {
    if (!fb || !fb->pixels || !out_buf) return 0;

    size_t pixel_bytes = (size_t)w * h * 4;
    size_t header_bytes = 16;
    if (header_bytes + pixel_bytes > max_len) return 0;

    out_buf[0] = 0; out_buf[1] = 0; out_buf[2] = 0; out_buf[3] = 1;
    out_buf[4] = (x >> 8) & 0xFF; out_buf[5] = x & 0xFF;
    out_buf[6] = (y >> 8) & 0xFF; out_buf[7] = y & 0xFF;
    out_buf[8] = (w >> 8) & 0xFF; out_buf[9] = w & 0xFF;
    out_buf[10] = (h >> 8) & 0xFF; out_buf[11] = h & 0xFF;
    out_buf[12] = 0; out_buf[13] = 0; out_buf[14] = 0; out_buf[15] = 0;

    uint32_t *src = (uint32_t *)fb->pixels;
    uint32_t *dst = (uint32_t *)(out_buf + 16);

    for (uint16_t r = 0; r < h; r++) {
        memcpy(dst + (r * w), src + ((y + r) * fb->width + x), w * 4);
    }
    return 16 + pixel_bytes;
}
