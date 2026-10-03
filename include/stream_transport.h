#ifndef MAR4UDER_STREAM_TRANSPORT_H
#define MAR4UDER_STREAM_TRANSPORT_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/**
 * Start ultra-low-latency WebRTC/RTSP desktop stream.
 * @param server_host  Target RTSP server hostname/IP (e.g. "104.143.206.163")
 * @param rtsp_port    RTSP ingress port (default 8554)
 * @param stream_name  Stream identifier (e.g. "desktop" or node_id)
 * @return 0 on success, -1 on failure
 */
int  stream_transport_start(const char *server_host, uint16_t rtsp_port, const char *stream_name);
int  stream_transport_start_ext(const char *server_host, uint16_t rtsp_port, const char *stream_name,
                                uint16_t width, uint16_t height, uint16_t fps, uint16_t bitrate_kb, uint8_t preset);

/**
 * Stop active video stream and terminate encoder process cleanly.
 */
void stream_transport_stop(void);

/**
 * Check if stream transport is currently active.
 * @return 1 if active, 0 if inactive
 */
int  stream_transport_is_active(void);

/**
 * Regular watchdog tick called from agent event loop.
 * Monitors child process health and auto-restarts if display geometry changes.
 */
void stream_transport_tick(void);

/**
 * Query native screen resolution of the active X11/framebuffer display.
 */
void stream_transport_get_resolution(int *out_w, int *out_h);

#ifdef __cplusplus
}
#endif

#endif /* MAR4UDER_STREAM_TRANSPORT_H */
