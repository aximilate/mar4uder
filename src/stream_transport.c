#define _GNU_SOURCE
#include "stream_transport.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <signal.h>
#include <fcntl.h>
#include <dlfcn.h>
#include <dirent.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <sys/stat.h>
#include <time.h>

static pid_t    g_stream_pid    = -1;
static int      g_stream_active = 0;
static char     g_srv_host[64]  = {0};
static uint16_t g_rtsp_port     = 8554;
static char     g_stream_name[64] = "desktop";
static int      g_cur_width     = 1920;
static int      g_cur_height    = 1080;
static int      g_req_width     = 0;
static int      g_req_height    = 0;
static int      g_req_fps       = 30;
static int      g_req_bitrate   = 2000;
static uint8_t  g_req_preset    = 0;
static uint64_t g_last_tick_ms  = 0;

static uint64_t get_time_ms(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000 + (uint64_t)ts.tv_nsec / 1000000;
}

static void find_xauth_from_proc(char *out_xauth, size_t auth_len) {
    DIR *pd = opendir("/proc");
    if (!pd) return;
    struct dirent *de;
    while ((de = readdir(pd)) != NULL) {
        if (de->d_name[0] < '0' || de->d_name[0] > '9') continue;
        char cpath[128];
        snprintf(cpath, sizeof(cpath), "/proc/%s/cmdline", de->d_name);
        int fd = open(cpath, O_RDONLY);
        if (fd < 0) continue;
        char buf[1024];
        ssize_t n = read(fd, buf, sizeof(buf) - 1);
        close(fd);
        if (n <= 0) continue;
        buf[n] = '\0';
        for (ssize_t i = 0; i < n - 6; i++) {
            if (strcmp(buf + i, "-auth") == 0) {
                size_t path_offset = i + strlen("-auth") + 1;
                if (path_offset < (size_t)n) {
                    const char *cand = buf + path_offset;
                    if (access(cand, R_OK) == 0) {
                        snprintf(out_xauth, auth_len, "%s", cand);
                        closedir(pd);
                        return;
                    }
                }
            }
        }
    }
    closedir(pd);
}

static void ensure_x11_env(void) {
    if (!getenv("DISPLAY")) {
        setenv("DISPLAY", ":0", 0);
    }

    if (getenv("XAUTHORITY") && access(getenv("XAUTHORITY"), R_OK) == 0) {
        return;
    }

    /* 1. Try reading -auth from running Xorg/X /proc/ cmdline */
    char proc_auth[256];
    proc_auth[0] = '\0';
    find_xauth_from_proc(proc_auth, sizeof(proc_auth));
    if (proc_auth[0] != '\0') {
        setenv("XAUTHORITY", proc_auth, 1);
        return;
    }

    /* 2. Common Xauthority locations */
    const char *dirs[] = {"/tmp", "/run/sddm", "/var/run/sddm", "/run/user/500", "/run/user/1000", "/run/user", NULL};
    for (int i = 0; dirs[i]; i++) {
        DIR *d = opendir(dirs[i]);
        if (d) {
            struct dirent *de;
            while ((de = readdir(d)) != NULL) {
                if (strstr(de->d_name, "auth") != NULL) {
                    char path[256];
                    snprintf(path, sizeof(path), "%s/%s", dirs[i], de->d_name);
                    if (access(path, R_OK) == 0) {
                        setenv("XAUTHORITY", path, 1);
                        closedir(d);
                        return;
                    }
                }
            }
            closedir(d);
        }
    }

    if (access("/root/.Xauthority", R_OK) == 0) {
        setenv("XAUTHORITY", "/root/.Xauthority", 1);
        return;
    }

    DIR *hd = opendir("/home");
    if (hd) {
        struct dirent *de;
        while ((de = readdir(hd)) != NULL) {
            if (de->d_name[0] != '.') {
                char test_path[256];
                snprintf(test_path, sizeof(test_path), "/home/%s/.Xauthority", de->d_name);
                if (access(test_path, R_OK) == 0) {
                    setenv("XAUTHORITY", test_path, 1);
                    closedir(hd);
                    return;
                }
            }
        }
        closedir(hd);
    }
}

/* Dynamically query X11 root window dimensions without hard build-time link */
static void detect_screen_resolution(int *out_w, int *out_h) {
    ensure_x11_env();

    int w = 1920, h = 1080;

    void *x11 = dlopen("libX11.so.6", RTLD_LAZY);
    if (!x11) x11 = dlopen("libX11.so", RTLD_LAZY);

    if (x11) {
        typedef void* (*XOpenDisplay_fn)(const char*);
        typedef int   (*XCloseDisplay_fn)(void*);
        typedef int   (*XDisplayWidth_fn)(void*, int);
        typedef int   (*XDisplayHeight_fn)(void*, int);
        typedef int   (*XDefaultScreen_fn)(void*);

        XOpenDisplay_fn   fn_open   = (XOpenDisplay_fn)dlsym(x11, "XOpenDisplay");
        XCloseDisplay_fn  fn_close  = (XCloseDisplay_fn)dlsym(x11, "XCloseDisplay");
        XDisplayWidth_fn  fn_width  = (XDisplayWidth_fn)dlsym(x11, "XDisplayWidth");
        XDisplayHeight_fn fn_height = (XDisplayHeight_fn)dlsym(x11, "XDisplayHeight");
        XDefaultScreen_fn fn_screen = (XDefaultScreen_fn)dlsym(x11, "XDefaultScreen");

        if (fn_open && fn_close && fn_width && fn_height && fn_screen) {
            const char *disp_env = getenv("DISPLAY");
            void *disp = fn_open(disp_env ? disp_env : ":0");
            if (disp) {
                int scr = fn_screen(disp);
                int dw = fn_width(disp, scr);
                int dh = fn_height(disp, scr);
                if (dw > 0 && dh > 0) {
                    w = dw;
                    h = dh;
                }
                fn_close(disp);
            }
        }
        dlclose(x11);
    } else {
        /* Fallback: parse /sys/class/graphics/fb0/virtual_size */
        FILE *fp = fopen("/sys/class/graphics/fb0/virtual_size", "r");
        if (fp) {
            int fw = 0, fh = 0;
            if (fscanf(fp, "%d,%d", &fw, &fh) == 2 && fw > 0 && fh > 0) {
                w = fw;
                h = fh;
            }
            fclose(fp);
        }
    }

    /* Force even dimensions for H.264 macroblocks */
    w -= (w % 2);
    h -= (h % 2);

    if (w < 320)  w = 1280;
    if (h < 240)  h = 720;

    *out_w = w;
    *out_h = h;
}

static int spawn_encoder_process(void) {
    ensure_x11_env();
    detect_screen_resolution(&g_cur_width, &g_cur_height);

    const char *display = getenv("DISPLAY");
    if (!display || display[0] == '\0') {
        display = ":0";
    }

    char res_str[32];
    snprintf(res_str, sizeof(res_str), "%dx%d", g_cur_width, g_cur_height);

    char rtsp_url[256];
    snprintf(rtsp_url, sizeof(rtsp_url), "rtsp://%s:%u/%s",
             g_srv_host, (unsigned int)g_rtsp_port, g_stream_name);

    int fps = (g_req_fps >= 5 && g_req_fps <= 60) ? g_req_fps : 30;
    char fps_str[16];
    snprintf(fps_str, sizeof(fps_str), "%d", fps);

    char gop_str[16];
    snprintf(gop_str, sizeof(gop_str), "%d", fps);

    int br = (g_req_bitrate >= 100 && g_req_bitrate <= 10000) ? g_req_bitrate : 2000;
    char b_str[32];
    snprintf(b_str, sizeof(b_str), "%dk", br);
    char maxrate_str[32];
    snprintf(maxrate_str, sizeof(maxrate_str), "%dk", (int)(br * 1.5));
    char bufsize_str[32];
    snprintf(bufsize_str, sizeof(bufsize_str), "%dk", (int)(br * 1.5));

    const char *preset_str = "ultrafast";
    if (g_req_preset == 1) preset_str = "superfast";
    else if (g_req_preset == 2) preset_str = "veryfast";

    char vf_str[256];
    if (g_req_width > 0 && g_req_height > 0) {
        int tw = g_req_width - (g_req_width % 2);
        int th = g_req_height - (g_req_height % 2);
        if (tw < 320) tw = 320;
        if (th < 240) th = 240;
        snprintf(vf_str, sizeof(vf_str),
                 "crop=trunc(iw/2)*2:trunc(ih/2)*2:0:0,scale=%d:%d:flags=fast_bilinear,format=yuv420p",
                 tw, th);
    } else {
        snprintf(vf_str, sizeof(vf_str), "crop=trunc(iw/2)*2:trunc(ih/2)*2:0:0,format=yuv420p");
    }

    /* Hardware VAAPI is only enabled when explicitly opted-in via M4R_VAAPI=1,
       otherwise libx264 ultrafast/zerolatency is used as it is universally compatible
       across VMs (VirtualBox/VMware/KVM) and hardware with negligible CPU usage (<5%). */
    int has_vaapi = 0;
    if (getenv("M4R_VAAPI") && access("/dev/dri/renderD128", R_OK | W_OK) == 0) {
        has_vaapi = 1;
    }

    printf("[+] Stream Transport: spawning ffmpeg (%s, res=%s, fps=%d, br=%dk, preset=%s)\n",
           has_vaapi ? "VAAPI hardware" : "libx264",
           res_str, fps, br, preset_str);

    pid_t pid = fork();
    if (pid < 0) {
        perror("[-] Stream Transport: fork failed");
        return -1;
    }

    if (pid == 0) {
        /* Child process */
        /* Redirect stdout/stderr to log file for debugging */
        int fd = open("/tmp/m4r_stream.log", O_WRONLY | O_CREAT | O_TRUNC, 0644);
        if (fd >= 0) {
            dup2(fd, STDOUT_FILENO);
            dup2(fd, STDERR_FILENO);
            close(fd);
        } else {
            int devnull = open("/dev/null", O_WRONLY);
            if (devnull >= 0) {
                dup2(devnull, STDOUT_FILENO);
                dup2(devnull, STDERR_FILENO);
                close(devnull);
            }
        }

        if (has_vaapi) {
            char *argv[] = {
                "ffmpeg",
                "-hide_banner",
                "-loglevel", "warning",
                "-thread_queue_size", "1024",
                "-f", "x11grab",
                "-draw_mouse", "1",
                "-framerate", fps_str,
                "-i", (char *)display,
                "-vaapi_device", "/dev/dri/renderD128",
                "-vf", "format=nv12,hwupload",
                "-c:v", "h264_vaapi",
                "-profile:v", "66",
                "-bf", "0",
                "-g", gop_str,
                "-f", "rtsp",
                "-rtsp_transport", "tcp",
                rtsp_url,
                NULL
            };
            execvp("ffmpeg", argv);
        } else {
            char *argv[] = {
                "ffmpeg",
                "-hide_banner",
                "-loglevel", "warning",
                "-thread_queue_size", "1024",
                "-f", "x11grab",
                "-draw_mouse", "1",
                "-framerate", fps_str,
                "-i", (char *)display,
                "-vf", vf_str,
                "-c:v", "libx264",
                "-preset", (char *)preset_str,
                "-tune", "zerolatency",
                "-profile:v", "baseline",
                "-b:v", b_str,
                "-maxrate", maxrate_str,
                "-bufsize", bufsize_str,
                "-g", gop_str,
                "-keyint_min", gop_str,
                "-sc_threshold", "0",
                "-flags", "+low_delay",
                "-bsf:v", "dump_extra=freq=keyframe",
                "-f", "rtsp",
                "-rtsp_transport", "tcp",
                rtsp_url,
                NULL
            };
            execvp("ffmpeg", argv);
        }

        _exit(127);
    }

    g_stream_pid = pid;
    return 0;
}

int stream_transport_start_ext(const char *server_host, uint16_t rtsp_port, const char *stream_name,
                                uint16_t width, uint16_t height, uint16_t fps, uint16_t bitrate_kb, uint8_t preset) {
    if (!server_host || server_host[0] == '\0') {
        return -1;
    }

    const char *target_stream = (stream_name && stream_name[0] != '\0') ? stream_name : "desktop";
    uint16_t target_port = (rtsp_port > 0) ? rtsp_port : 8554;
    int target_fps = (fps > 0) ? fps : 30;
    int target_br = (bitrate_kb > 0) ? bitrate_kb : 2000;

    /* Check if already running healthy with identical parameters */
    if (g_stream_active && g_stream_pid > 0 && kill(g_stream_pid, 0) == 0) {
        if (g_req_width == width && g_req_height == height &&
            g_req_fps == target_fps && g_req_bitrate == target_br && g_req_preset == preset &&
            g_rtsp_port == target_port &&
            strcmp(g_srv_host, server_host) == 0 &&
            strcmp(g_stream_name, target_stream) == 0) {
            return 0; /* Keep active without interruption */
        }
    }

    /* Stop previous stream if active */
    stream_transport_stop();

    snprintf(g_srv_host, sizeof(g_srv_host), "%s", server_host);
    g_rtsp_port = target_port;
    snprintf(g_stream_name, sizeof(g_stream_name), "%s", target_stream);

    g_req_width = width;
    g_req_height = height;
    g_req_fps = target_fps;
    g_req_bitrate = target_br;
    g_req_preset = preset;

    g_stream_active = 1;
    return spawn_encoder_process();
}

int stream_transport_start(const char *server_host, uint16_t rtsp_port, const char *stream_name) {
    return stream_transport_start_ext(server_host, rtsp_port, stream_name, 0, 0, 30, 2000, 0);
}

void stream_transport_stop(void) {
    g_stream_active = 0;

    if (g_stream_pid > 0) {
        kill(g_stream_pid, SIGTERM);
        usleep(50000); /* 50ms */
        kill(g_stream_pid, SIGKILL);
        waitpid(g_stream_pid, NULL, WNOHANG);
        g_stream_pid = -1;
    }
}

static int      g_stream_fail_count = 0;
static uint64_t g_last_spawn_time   = 0;

static int is_ffmpeg_available(void) {
    return (access("/usr/bin/ffmpeg", X_OK) == 0 ||
            access("/usr/local/bin/ffmpeg", X_OK) == 0 ||
            system("which ffmpeg >/dev/null 2>&1") == 0);
}

void stream_transport_tick(void) {
    if (!g_stream_active) {
        return;
    }

    uint64_t now = get_time_ms();
    if (now - g_last_tick_ms < 2000) {
        return;
    }
    g_last_tick_ms = now;

    if (!is_ffmpeg_available()) {
        printf("[-] Stream Transport: ffmpeg is not installed on target system. Aborting stream.\n");
        g_stream_active = 0;
        return;
    }

    if (g_stream_pid > 0) {
        int status = 0;
        pid_t res = waitpid(g_stream_pid, &status, WNOHANG);
        if (res > 0) {
            uint64_t run_duration = now - g_last_spawn_time;
            g_stream_pid = -1;

            if (run_duration < 1500) {
                g_stream_fail_count++;
            } else {
                g_stream_fail_count = 0;
            }

            if (g_stream_fail_count >= 5) {
                printf("[-] Stream Transport: ffmpeg exited 5 times in quick succession. Halting auto-respawn.\n");
                g_stream_active = 0;
                return;
            }

            printf("[!] Stream Transport: child exited (code=%d, run=%lums, fails=%d), restarting...\n",
                   WIFEXITED(status) ? WEXITSTATUS(status) : -1,
                   (unsigned long)run_duration, g_stream_fail_count);
            g_last_spawn_time = now;
            spawn_encoder_process();
        }
    } else {
        g_last_spawn_time = now;
        spawn_encoder_process();
    }
}

void stream_transport_get_resolution(int *out_w, int *out_h) {
    detect_screen_resolution(out_w, out_h);
}
