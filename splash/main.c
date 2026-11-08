/* hadron-splash — animated hadron-desktop boot splash, installed as
 * /usr/bin/kairos-splash so Kairos runs it in both halves of the boot.
 *
 * Aesthetic: neon-noir cyberpunk in a Tokyo Night blue/cyan ramp. The machine
 * wakes: the logo glitches in, a CRT scanline sweeps it, and terse boot lines
 * type out beneath.
 *
 * Two renderers share that design:
 *   - graphics (gfx_run): pixels straight into /dev/fb0, the framebuffer every
 *     DRM driver exposes through fbdev emulation, over the same gradient the
 *     GRUB theme paints, so GRUB -> splash -> login reads as one screen;
 *   - text (text_run): the VGA-16 console animation, for a VT with no
 *     framebuffer.
 * With no terminal at all it prints one line and exits.
 *
 * The contract is kairos-init's (kairos-io/kairos internal/splash): exec'd with
 * no arguments in the initramfs, where it runs until SIGTERM at switch-root, and
 * with --duration=3s on the booted system, where getty or the display manager
 * waits for it. kairos.splash=0 on the kernel command line turns it off.
 * C99, libc and Linux headers only.
 */
#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif
#ifndef _DEFAULT_SOURCE
#define _DEFAULT_SOURCE
#endif
#include <signal.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <math.h>
#include <linux/fb.h>
#include <linux/kd.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/un.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>

#include "agent.h"
#include "font.h"

#define FRAME_USEC   33333
#define MIN_COLS     58
#define MIN_ROWS     12
#define GLITCH_SEC   1.10   /* logo resolves out of glitch over this window */

/* Tokyo Night ramp, dim -> hot. The kernel VT/framebuffer is a 16-color VGA
   console: the truecolor Tokyo Night values (#7aa2f7, #7dcfff, #565f89) have no
   256-color equivalent that survives the round-trip, so we use the colors the
   VGA palette actually has — 4 = blue, 8 = dark grey (the #565f89 comment hue),
   12 = bright blue (#7aa2f7), 14 = bright cyan (#7dcfff), 15 = white-hot. */
static const uint8_t TOKYO[8] = {4, 4, 8, 12, 12, 12, 14, 15};
#define A_CHROME 1   /* faint HUD brackets / leaders */
#define A_DIM    2   /* tagline, version */
#define A_BODY   4   /* logo at rest */
#define A_BRIGHT 6   /* scanline shoulder */
#define A_HOT    7   /* scanline crest, glitch sparks, cursor */

static char g_ver[64] = "";

/* The hadron-desktop release, written by the Dockerfile into the branding
   directory kairos-init's dracut module copies into the initramfs, so both
   halves of the boot show the same string. os-release (the Hadron base) is
   the fallback for an image built without it. */
static void read_kv(const char *path, const char *key) {
    FILE *f = fopen(path, "r");
    if (!f) return;
    char b[256]; size_t kl = strlen(key);
    while (fgets(b, sizeof(b), f)) {
        if (key[0] && strncmp(b, key, kl)) continue;
        char *v = b + kl; if (*v == '"') v++;
        size_t L = strlen(v);
        while (L && (v[L-1] == '\n' || v[L-1] == '\r' || v[L-1] == '"')) v[--L] = 0;
        if (L) snprintf(g_ver, sizeof(g_ver), "%s%.40s", v[0] == 'v' ? "" : "v", v);
        break;
    }
    fclose(f);
}
static void load_version(void) {
    read_kv("/etc/kairos/branding/splash/version", "");
    if (!g_ver[0]) read_kv("/etc/os-release", "VERSION_ID=");
}

typedef struct { char g[4]; uint8_t color; } cell_t;
typedef struct { int rows, cols; cell_t *cells; } grid_t;

static int g_alloc(grid_t *g, int r, int c) {
    g->rows = r; g->cols = c;
    g->cells = calloc((size_t)r * c, sizeof *g->cells);
    if (!g->cells) return -1;
    for (int i = 0; i < r * c; i++) g->cells[i].g[0] = ' ';
    return 0;
}
static void g_clear(grid_t *g) {
    for (int i = 0; i < g->rows * g->cols; i++) {
        g->cells[i].g[0] = ' '; g->cells[i].g[1] = 0; g->cells[i].color = 0;
    }
}
static void g_set(grid_t *g, int r, int c, const char *s, uint8_t col) {
    if (r < 0 || r >= g->rows || c < 0 || c >= g->cols) return;
    cell_t *x = &g->cells[r * g->cols + c];
    int i = 0; while (i < 3 && s[i]) { x->g[i] = s[i]; i++; } x->g[i] = 0;
    x->color = col;
}

static int utf8_next(const char *s, char o[4]) {
    unsigned c = (unsigned char)s[0];
    if (!c) { o[0] = 0; return 0; }
    int n = c < 0x80 ? 1 : (c & 0xE0) == 0xC0 ? 2 : (c & 0xF0) == 0xE0 ? 3 : 4;
    for (int i = 0; i < n; i++) o[i] = s[i];
    o[n < 4 ? n : 3] = 0;
    return n;
}
static int utf8_cols(const char *s) {
    int n = 0; char t[4];
    while (*s) { int a = utf8_next(s, t); if (!a) break; s += a; n++; }
    return n;
}

#define ART_ROWS 6
#define ART_COLS 51
static const char *ART[ART_ROWS] = {
"██╗  ██╗ █████╗ ██████╗ ██████╗  ██████╗ ███╗   ██╗",
"██║  ██║██╔══██╗██╔══██╗██╔══██╗██╔═══██╗████╗  ██║",
"███████║███████║██║  ██║██████╔╝██║   ██║██╔██╗ ██║",
"██╔══██║██╔══██║██║  ██║██╔══██╗██║   ██║██║╚██╗██║",
"██║  ██║██║  ██║██████╔╝██║  ██║╚██████╔╝██║ ╚████║",
"╚═╝  ╚═╝╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝",
};
static const char *TAG = "the immutable desktop \u00b7 built from source on Kairos";
/* In-progress, honest boot phrases — true during early boot, never claiming a
   service is "ready" before it is. */
static const char *BOOT[3] = {
    "waking the machine",
    "mounting the immutable core",
    "bringing up the desktop",
};

static uint32_t xs(uint32_t *s) { uint32_t x = *s; x ^= x<<13; x ^= x>>17; x ^= x<<5; *s = x?x:0xdeadbeef; return *s; }
static double   xu(uint32_t *s) { return (xs(s) & 0xFFFFFF) / (double)0x1000000; }

/* paint_logo: the HADRON wordmark with a downward CRT scanline (crest = white-
   hot, shoulders bright cyan) over a steady blue body, plus a glitch-in dissolve
   while gl>0 (random glyph corruption + 1-col jitter that thins out as gl->0). */
static const char GLITCH_POOL[] = "#%&/\\|=+<>*!?";
static void paint_logo(grid_t *g, int top, int left, int scan, int flick,
                       double gl, uint32_t *rng) {
    for (int r = 0; r < ART_ROWS; r++) {
        const char *p = ART[r]; int c = 0; char glx[4];
        while (*p) {
            int a = utf8_next(p, glx); if (!a) break;
            if (!(a == 1 && glx[0] == ' ')) {
                int d = r - scan; if (d < 0) d = -d;
                int ci = d == 0 ? A_HOT : d == 1 ? A_BRIGHT : A_BODY;
                const char *put = glx; char buf[4];
                if (gl > 0 && xu(rng) < gl * 0.32) {
                    buf[0] = GLITCH_POOL[xs(rng) % (sizeof(GLITCH_POOL) - 1)];
                    buf[1] = 0; put = buf; ci = A_HOT;   /* sparks read brightest */
                }
                int cc = c;
                if (gl > 0 && xu(rng) < gl * 0.12) cc += (int)(xs(rng) % 3) - 1;
                if (flick && ci > 0) ci--;               /* phosphor flicker dip */
                g_set(g, top + r, left + cc, put, TOKYO[ci]);
            }
            p += a; c++;
        }
    }
}

static void paint_center(grid_t *g, int row, const char *s, uint8_t col) {
    if (!s || !*s) return;
    int left = (g->cols - utf8_cols(s)) / 2; if (left < 0) left = 0;
    int c = 0; char gl[4];
    while (*s) { int a = utf8_next(s, gl); if (!a) break;
        g_set(g, row, left + c, gl, col); s += a; c++; }
}

/* corner brackets framing the block — a faint blue HUD, drawn with the same
   double-line glyphs the logo proves the VT font renders. */
static void paint_brackets(grid_t *g, int t, int l, int b, int r, uint8_t col) {
    g_set(g, t, l,   "╔", col); g_set(g, t, l+1, "═", col); g_set(g, t+1, l, "║", col);
    g_set(g, t, r,   "╗", col); g_set(g, t, r-1, "═", col); g_set(g, t+1, r, "║", col);
    g_set(g, b, l,   "╚", col); g_set(g, b, l+1, "═", col); g_set(g, b-1, l, "║", col);
    g_set(g, b, r,   "╝", col); g_set(g, b, r-1, "═", col); g_set(g, b-1, r, "║", col);
}

static double now_sec(void) {
    struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec + t.tv_nsec / 1e9;
}

/* Both halves of the boot run one animation: the initramfs half records when
   it started in /run, which survives switch-root, and the booted half picks up
   the same timeline instead of replaying the glitch-in. CLOCK_MONOTONIC counts
   from boot, so the two processes agree on it. */
#define TIMELINE "/run/hadron-splash.start"
static double timeline_start(void) {
    double now = now_sec(), t0 = 0;
    FILE *f = fopen(TIMELINE, "r");
    if (f) {
        if (fscanf(f, "%lf", &t0) != 1 || t0 > now || now - t0 > 120) t0 = 0;
        fclose(f);
    }
    if (t0 > 0) return t0;
    if ((f = fopen(TIMELINE, "w"))) { fprintf(f, "%.3f\n", now); fclose(f); }
    return now;
}

static int cell_eq(const cell_t *a, const cell_t *b) {
    return a->color == b->color && !strcmp(a->g, b->g);
}
static void flush_grid(const grid_t *cur, grid_t *prev) {
    int sgr = -1;
    for (int r = 0; r < cur->rows; r++) for (int c = 0; c < cur->cols; c++) {
        int i = r * cur->cols + c;
        cell_t a = cur->cells[i], b = prev->cells[i];
        if (cell_eq(&a, &b)) continue;
        printf("\033[%d;%dH", r+1, c+1);
        int w = a.color;
        // Plain 16-color SGR only: 30-37 for 0-7, 90-97 for 8-15. The 256-color
        // form (38;5;N) is not honoured by every early-boot console we render on,
        // and no forced bold — bold would promote 4 (blue) to 12 (bright blue) and
        // flatten the dim->hot ramp. 12/14/15 are already bright.
        if (w != sgr) {
            if (!w) fputs("\033[0m", stdout);
            else printf("\033[0;%dm", w < 8 ? 30 + w : 90 + (w - 8));
            sgr = w;
        }
        fputs(a.g[0] ? a.g : " ", stdout);
        prev->cells[i] = a;
    }
    fflush(stdout);
}

static volatile sig_atomic_t g_exit = 0;
static void on_sig(int s) { (void)s; g_exit = 1; }

static int text_run(double duration) {
    struct winsize ws; int rows = 24, cols = 80;
    if (ioctl(STDOUT_FILENO, TIOCGWINSZ, &ws) == 0 && ws.ws_row && ws.ws_col) {
        rows = ws.ws_row; cols = ws.ws_col;
    }
    if (rows < MIN_ROWS || cols < MIN_COLS) return 0;

    grid_t cur, prev;
    if (g_alloc(&cur, rows, cols) || g_alloc(&prev, rows, cols)) return 1;

    fputs("\033[?1049h\033[?25l\033[2J\033[H", stdout); fflush(stdout);

    int block_h = ART_ROWS + 4;               /* logo + tag + ver + status */
    int top     = (rows - block_h) / 2;
    int left    = (cols - ART_COLS) / 2;
    int tag_row = top + ART_ROWS + 1;
    int ver_row = top + ART_ROWS + 2;
    int sts_row = top + ART_ROWS + 3;
    int fr_l = left - 3, fr_r = left + ART_COLS + 2;
    int fr_t = top - 1,  fr_b = sts_row + 1;

    double t0 = now_sec();
    int frame = 0;
    while (!g_exit) {
        double tn = now_sec(), t = tn - t0;
        if (duration > 0 && t >= duration) break;

        uint32_t rng = (uint32_t)(frame * 2654435761u) ^ 0x9e3779b9u;
        g_clear(&cur);

        int flick = (xu(&rng) < 0.05) ? 1 : 0;                 /* rare CRT flicker */
        int scan  = ((int)(t * 9.0)) % (ART_ROWS + 4) - 2;     /* sweep down, wrap */
        double gl = t < GLITCH_SEC ? (1.0 - t / GLITCH_SEC) : 0.0;

        paint_brackets(&cur, fr_t, fr_l, fr_b, fr_r, TOKYO[A_CHROME]);
        paint_logo(&cur, top, left, scan, flick, gl, &rng);
        paint_center(&cur, tag_row, TAG, TOKYO[flick ? A_CHROME : A_DIM]);
        if (g_ver[0]) paint_center(&cur, ver_row, g_ver, TOKYO[A_CHROME]);

        /* typewritten boot line, cycling through the phrases, blinking cursor */
        if (t > GLITCH_SEC * 0.5) {
            double bt = t - GLITCH_SEC * 0.5;
            int bi = (int)(bt / 1.40); if (bi > 2) bi = 2;
            const char *msg = BOOT[bi];
            int shown = (int)((bt - bi * 1.40) * 24.0);
            int tot = (int)strlen(msg); if (shown > tot) shown = tot; if (shown < 0) shown = 0;
            int sl = (cols - (tot + 4)) / 2; if (sl < 0) sl = 0;
            g_set(&cur, sts_row, sl, ">", TOKYO[A_DIM]);
            for (int i = 0; i < shown; i++) { char ch[2] = { msg[i], 0 }; g_set(&cur, sts_row, sl + 2 + i, ch, TOKYO[A_BODY]); }
            if (((int)(t * 2)) & 1) g_set(&cur, sts_row, sl + 2 + shown, "_", TOKYO[A_HOT]);
        }

        /* Differential renderer: force a periodic full repaint so a stray
           boot-log line the kernel/udev wrote onto our tty gets overwritten by
           our own cells (spaces included). No clear -> no flicker. */
        if (++frame % 15 == 0)
            for (int i = 0; i < prev.rows * prev.cols; i++) prev.cells[i].g[0] = '\x01';
        flush_grid(&cur, &prev);
        usleep(FRAME_USEC);
    }

    /* Leave the alt screen and wipe the primary buffer + scrollback so the
       handoff (switch-root, or the login prompt) starts clean. */
    fputs("\033[0m\033[?25h\033[?1049l\033[H\033[2J\033[3J", stdout); fflush(stdout);
    free(cur.cells); free(prev.cells);
    return 0;
}

/* ======================================================================== */
/* Graphics: draw into a framebuffer.                                        */
/* ======================================================================== */

/* Tokyo Night in truecolor, which the framebuffer can show. */
#define C_BG_TOP   0x1a1b26
#define C_BG_BOT   0x16161e
#define C_GLOW     0x7aa2f7
#define C_BODY     0x7aa2f7
#define C_CYAN     0x7dcfff
#define C_HOT      0xc0caf5
#define C_SHADOW   0x414868
#define C_DIM      0x565f89
#define C_TEXT     0xa9b1d6
/* The passphrase panel. */
#define C_PANEL    0x24283b
#define C_FIELD    0x292e42
#define C_LABEL    0x737aa2
#define C_ERROR    0xf7768e

/* A draw target: the mmapped framebuffer, or a heap buffer for --render.
   Pixels are composed in an XRGB back buffer and packed into the target's
   own layout (32 or 16 bpp, any channel offsets) when blitted. */
typedef struct {
    int fd;
    uint8_t *mem; size_t len;
    int w, h, stride, bpp;
    struct fb_bitfield r, g, b;
    uint32_t *back;   /* w*h XRGB */
    uint32_t *bg;     /* w*h XRGB: the static background */
    char id[16];      /* fb_fix_screeninfo.id: which driver owns the device */
    unsigned long smem_start;
} canvas_t;

static uint32_t mix(uint32_t a, uint32_t b, double t) {
    if (t <= 0) return a;
    if (t >= 1) return b;
    int ar = a >> 16 & 255, ag = a >> 8 & 255, ab = a & 255;
    int br = b >> 16 & 255, bg = b >> 8 & 255, bb = b & 255;
    return (uint32_t)((int)(ar + (br - ar) * t + .5) << 16 |
                      (int)(ag + (bg - ag) * t + .5) << 8 |
                      (int)(ab + (bb - ab) * t + .5));
}

/* tools/gen-grub-bg.py's formula, evaluated at this resolution: a vertical
   bg -> bg_dark gradient with a faint blue glow centred above the screen. */
static void paint_background(canvas_t *c) {
    double cx = c->w * .5, cy = -c->h * .15, radius = c->h * .95;
    for (int y = 0; y < c->h; y++) {
        uint32_t base = mix(C_BG_TOP, C_BG_BOT, c->h > 1 ? (double)y / (c->h - 1) : 0);
        for (int x = 0; x < c->w; x++) {
            double dx = x - cx, dy = y - cy, d = sqrt(dx * dx + dy * dy);
            double g = 1.0 - d / radius; g = g > 0 ? g * g * .18 : 0;
            c->bg[(size_t)y * c->w + x] = mix(base, C_GLOW, g);
        }
    }
}

static uint32_t pack(const canvas_t *c, uint32_t px) {
    uint32_t r = px >> 16 & 255, g = px >> 8 & 255, b = px & 255;
    return (r >> (8 - c->r.length)) << c->r.offset |
           (g >> (8 - c->g.length)) << c->g.offset |
           (b >> (8 - c->b.length)) << c->b.offset;
}

/* Copy rows [y0, y1) of the back buffer to the target. */
static void blit(canvas_t *c, int y0, int y1) {
    if (y0 < 0) y0 = 0;
    if (y1 > c->h) y1 = c->h;
    for (int y = y0; y < y1; y++) {
        const uint32_t *src = c->back + (size_t)y * c->w;
        uint8_t *dst = c->mem + (size_t)y * c->stride;
        if (c->bpp == 32 && c->r.offset == 16 && c->g.offset == 8 && c->b.offset == 0) {
            memcpy(dst, src, (size_t)c->w * 4);
        } else if (c->bpp == 32) {
            for (int x = 0; x < c->w; x++) ((uint32_t *)dst)[x] = pack(c, src[x]);
        } else {
            for (int x = 0; x < c->w; x++) ((uint16_t *)dst)[x] = (uint16_t)pack(c, src[x]);
        }
    }
}

static int canvas_alloc(canvas_t *c) {
    size_t n = (size_t)c->w * c->h;
    c->back = malloc(n * 4);
    c->bg = malloc(n * 4);
    if (!c->back || !c->bg) return -1;
    paint_background(c);
    memcpy(c->back, c->bg, n * 4);
    return 0;
}

static void canvas_free(canvas_t *c) {
    free(c->back); free(c->bg);
    c->back = c->bg = NULL;
    if (c->fd >= 0) {
        if (c->mem) munmap(c->mem, c->len);
        close(c->fd);
    }
    c->fd = -1; c->mem = NULL;
}

static int fb_open(canvas_t *c) {
    memset(c, 0, sizeof *c);
    c->fd = open("/dev/fb0", O_RDWR | O_CLOEXEC);
    if (c->fd < 0) return -1;
    struct fb_var_screeninfo v; struct fb_fix_screeninfo f;
    if (ioctl(c->fd, FBIOGET_VSCREENINFO, &v) || ioctl(c->fd, FBIOGET_FSCREENINFO, &f) ||
        (v.bits_per_pixel != 32 && v.bits_per_pixel != 16) ||
        f.type != FB_TYPE_PACKED_PIXELS || v.xres < 320 || v.yres < 200) {
        close(c->fd); c->fd = -1; return -1;
    }
    c->w = (int)v.xres; c->h = (int)v.yres; c->bpp = (int)v.bits_per_pixel;
    c->stride = (int)f.line_length; c->r = v.red; c->g = v.green; c->b = v.blue;
    c->len = f.smem_len;
    memcpy(c->id, f.id, sizeof c->id);
    c->smem_start = f.smem_start;
    c->mem = mmap(NULL, c->len, PROT_READ | PROT_WRITE, MAP_SHARED, c->fd, 0);
    if (c->mem == MAP_FAILED) { c->mem = NULL; canvas_free(c); return -1; }
    /* Put this framebuffer on screen. A DRM driver that has just replaced
       simpledrm registers its fbdev but leaves scanout alone until a client
       sets the mode, and with the VT in graphics mode fbcon never does: the
       screen would keep showing simpledrm's last frame while we draw into a
       buffer nobody displays. FORCE makes the driver commit even though the
       mode is unchanged. A failure here is harmless; the old content stays. */
    v.activate = FB_ACTIVATE_NOW | FB_ACTIVATE_FORCE;
    ioctl(c->fd, FBIOPUT_VSCREENINFO, &v);
    /* Start at the visible page, wherever the console left it. */
    c->mem += (size_t)v.yoffset * c->stride + (size_t)v.xoffset * (c->bpp / 8);
    c->len -= (size_t)v.yoffset * c->stride;
    if (canvas_alloc(c)) { canvas_free(c); return -1; }
    return 0;
}

/* A native DRM driver taking over from simpledrm (virtio-gpu, i915, amdgpu)
   replaces /dev/fb0 under us, often at the same resolution, and the old
   mapping silently stops being what the screen shows. So open the path again
   and compare who owns it, not just the geometry. */
static int fb_stale(const canvas_t *c) {
    struct fb_var_screeninfo v; struct fb_fix_screeninfo f;
    int fd = open("/dev/fb0", O_RDONLY | O_CLOEXEC);
    if (fd < 0) return 1;
    int bad = ioctl(fd, FBIOGET_VSCREENINFO, &v) || ioctl(fd, FBIOGET_FSCREENINFO, &f);
    close(fd);
    if (bad) return 1;
    return memcmp(f.id, c->id, sizeof c->id) || f.smem_start != c->smem_start ||
           (int)v.xres != c->w || (int)v.yres != c->h || (int)v.bits_per_pixel != c->bpp;
}

static void fill(canvas_t *c, int x, int y, int w, int h, uint32_t col) {
    if (x < 0) { w += x; x = 0; }
    if (y < 0) { h += y; y = 0; }
    if (x + w > c->w) w = c->w - x;
    if (y + h > c->h) h = c->h - y;
    for (int j = 0; j < h; j++) {
        uint32_t *row = c->back + (size_t)(y + j) * c->w + x;
        for (int i = 0; i < w; i++) row[i] = col;
    }
}

/* Restore the background over a rectangle. */
static void clear_rect(canvas_t *c, int x, int y, int w, int h) {
    if (x < 0) { w += x; x = 0; }
    if (y < 0) { h += y; y = 0; }
    if (x + w > c->w) w = c->w - x;
    if (y + h > c->h) h = c->h - y;
    for (int j = 0; j < h && w > 0; j++)
        memcpy(c->back + (size_t)(y + j) * c->w + x, c->bg + (size_t)(y + j) * c->w + x, (size_t)w * 4);
}

static int glyph_index(const char *s, int *adv) {
    unsigned char ch = (unsigned char)s[0];
    if (ch == 0xc2 && (unsigned char)s[1] == 0xb7) { *adv = 2; return 95; }
    *adv = 1;
    return ch >= 32 && ch < 127 ? ch - 32 : '?' - 32;
}

static int text_width(const char *s, int scale) {
    int n = 0, adv;
    while (*s) { glyph_index(s, &adv); s += adv; n++; }
    return n * FONT_W * scale;
}

static void draw_text(canvas_t *c, const char *s, int x, int y, int scale, uint32_t col) {
    int adv;
    while (*s) {
        const unsigned char *gl = FONT[glyph_index(s, &adv)];
        for (int r = 0; r < FONT_H; r++)
            for (int b = 0; b < FONT_W; b++)
                if (gl[r] & (0x80 >> b)) fill(c, x + b * scale, y + r * scale, scale, scale, col);
        x += FONT_W * scale; s += adv;
    }
}

typedef struct {
    int cw, ch;          /* art cell size */
    int ax, ay;          /* art origin */
    int ts;              /* text scale */
    int tag_y, ver_y, sts_y;
    int top, bottom;     /* rows that ever change */
    /* The passphrase panel, which takes the place of the tagline, version and
       status line while a question is open. */
    int ps;              /* panel text scale */
    int px, py, pw, ph;  /* panel box */
    int title_y, field_y, field_h, note_y;
} layout_t;

/* ======================================================================== */
/* Password agent: the disk passphrase prompt.                               */
/* ======================================================================== */

/* While the initramfs splash answers systemd's password questions it says so
   in this file (its PID), and the console agent's gate waits for that PID
   instead of asking on the console too. See
   rootfs/usr/lib/dracut/modules.d/51hadron-askpass/. */
#define AGENT_PIDDIR  "/run/hadron-ask"
#define AGENT_PIDFILE AGENT_PIDDIR "/splash-agent"

#define CHECKING_SEC 4.0   /* "Checking..." after Enter, until a retry comes */

typedef enum { P_NONE, P_ASK, P_CHECKING } pstate_t;

static struct {
    int on;                    /* registered as an agent */
    agent_t ag;
    pstate_t st;
    double checking_until;
    char title[64];
    int retry, echo, silent;
    char buf[512]; size_t len;
    int raw; struct termios saved;
} g_pr;

static int cmdline_has(const char *word);

static void prompt_wipe(void) {
    volatile char *p = g_pr.buf;
    for (size_t i = 0; i < sizeof g_pr.buf; i++) p[i] = 0;
    g_pr.len = 0;
}

static void agent_unregister(void) {
    if (!g_pr.on) return;
    unlink(AGENT_PIDFILE);
    if (g_pr.raw) tcsetattr(STDIN_FILENO, TCSANOW, &g_pr.saved);
    g_pr.raw = 0;
    agent_close(&g_pr.ag);
    prompt_wipe();
    g_pr.on = 0;
}

/* 1 = always (--agent), 0 = never (--no-agent, hadron.splash.agent=0),
   -1 = only in the initramfs. */
static int g_agent_mode = -1;

/* Answer questions only when the screen can show them and the keyboard can
   reach us: a framebuffer (the caller has one), the VT on stdin, and the
   right to write to the asker's socket. In the booted system the stock
   console agent asks, so the 3-second splash there stays out of it unless
   --agent says otherwise. */
static void agent_register(void) {
    if (g_agent_mode == 0 || cmdline_has("hadron.splash.agent=0")) return;
    if (g_agent_mode < 0 && access("/etc/initrd-release", F_OK)) return;
    if (!isatty(STDIN_FILENO) || tcgetattr(STDIN_FILENO, &g_pr.saved)) return;
    if (!ask_can_reply()) return;

    mkdir(AGENT_PIDDIR, 0755);
    char tmp[64];
    snprintf(tmp, sizeof tmp, "%s.%d", AGENT_PIDFILE, (int)getpid());
    FILE *f = fopen(tmp, "w");
    if (!f) return;
    fprintf(f, "%d\n", (int)getpid());
    if (fclose(f) || rename(tmp, AGENT_PIDFILE)) { unlink(tmp); return; }

    /* Raw input from the VT: one byte per key, no echo, no line editing,
       no signals from Ctrl-C. The kernel keymap still applies, so a German
       or Italian layout loaded by systemd-vconsole-setup types what its
       keys say. */
    struct termios t = g_pr.saved;
    t.c_lflag &= ~(tcflag_t)(ICANON | ECHO | ECHONL | ISIG | IEXTEN);
    t.c_iflag &= ~(tcflag_t)(IXON | ICRNL | INLCR | IGNCR | ISTRIP);
    t.c_cc[VMIN] = 0; t.c_cc[VTIME] = 0;
    g_pr.raw = tcsetattr(STDIN_FILENO, TCSANOW, &t) == 0;
    agent_init(&g_pr.ag, NULL);
    g_pr.on = 1;
    atexit(agent_unregister);
}

/* Tell systemd the agent decision is made. The initramfs drop-in makes the
   unit Type=notify, so the console agent, ordered after the splash, never
   starts before the PID file above exists or is known not to. */
static void notify_ready(void) {
    static int done;
    const char *s = getenv("NOTIFY_SOCKET");
    if (done || !s || !*s) return;
    done = 1;
    struct sockaddr_un sa; memset(&sa, 0, sizeof sa);
    sa.sun_family = AF_UNIX;
    size_t n = strlen(s);
    if (n >= sizeof sa.sun_path) return;
    memcpy(sa.sun_path, s, n);
    if (sa.sun_path[0] == '@') sa.sun_path[0] = 0;   /* abstract namespace */
    int fd = socket(AF_UNIX, SOCK_DGRAM | SOCK_CLOEXEC, 0);
    if (fd < 0) return;
    sendto(fd, "READY=1", 7, MSG_NOSIGNAL, (struct sockaddr *)&sa,
           (socklen_t)(offsetof(struct sockaddr_un, sun_path) + n));
    close(fd);
}

static void prompt_show(void) {
    ask_t *a = &g_pr.ag.cur;
    ask_title(a, g_pr.title, sizeof g_pr.title);
    g_pr.retry = ask_is_retry(a, g_pr.ag.last_id);
    g_pr.echo = a->echo; g_pr.silent = a->silent;
    prompt_wipe();
    /* Keys pressed before the question was on screen are not part of it. */
    tcflush(STDIN_FILENO, TCIFLUSH);
    g_pr.st = P_ASK;
}

static void prompt_submit(int cancel) {
    agent_answer(&g_pr.ag, cancel ? NULL : g_pr.buf);
    prompt_wipe();
    g_pr.st = cancel ? P_NONE : P_CHECKING;
    g_pr.checking_until = now_sec() + CHECKING_SEC;
}

/* Bytes from the VT. Enter answers, Backspace deletes one character (UTF-8
   aware), Esc and Ctrl-U clear, Ctrl-C cancels the question. Escape
   sequences (arrows, function keys, Alt+key) are skipped whole. */
static void prompt_keys(const unsigned char *b, size_t n) {
    for (size_t i = 0; i < n && g_pr.st == P_ASK; i++) {
        unsigned char c = b[i];
        if (c == 0x1b) {
            if (i + 1 < n) {                       /* a sequence, not the Esc key */
                i++;
                if (b[i] == '[' || b[i] == 'O') {
                    while (i + 1 < n && !(b[i + 1] >= 0x40 && b[i + 1] <= 0x7e)) i++;
                    if (i + 1 < n) i++;
                }
                continue;
            }
            prompt_wipe();
        } else if (c == '\r' || c == '\n') {
            prompt_submit(0);
        } else if (c == 0x7f || c == 0x08) {
            while (g_pr.len && ((unsigned char)g_pr.buf[g_pr.len - 1] & 0xC0) == 0x80) g_pr.buf[--g_pr.len] = 0;
            if (g_pr.len) g_pr.buf[--g_pr.len] = 0;
        } else if (c == 0x15) {
            prompt_wipe();
        } else if (c == 0x03) {
            prompt_submit(1);
        } else if (c >= 0x20 && g_pr.len + 1 < sizeof g_pr.buf) {
            g_pr.buf[g_pr.len++] = (char)c;
        }
    }
}

/* One step per frame: watch the directory, read the keyboard. */
static void prompt_tick(void) {
    if (!g_pr.on) return;
    double now = now_sec();
    int r = agent_poll(&g_pr.ag, now);
    if (r == 1) prompt_show();
    else if (r == 2 && g_pr.st == P_ASK) g_pr.st = P_NONE;
    if (g_pr.st == P_CHECKING && now > g_pr.checking_until) g_pr.st = P_NONE;
    if (g_pr.st == P_ASK) {
        unsigned char b[64];
        ssize_t n;
        while ((n = read(STDIN_FILENO, b, sizeof b)) > 0) prompt_keys(b, (size_t)n);
        memset(b, 0, sizeof b);
    }
}

static void fill_disc(canvas_t *c, int cx, int cy, int r, uint32_t col) {
    for (int dy = -r; dy <= r; dy++) {
        int hw = (int)(sqrt((double)r * r - (double)dy * dy) + .5);
        fill(c, cx - hw, cy + dy, 2 * hw + 1, 1, col);
    }
}

static void draw_center(canvas_t *c, const char *s, int y, int scale, uint32_t col) {
    draw_text(c, s, (c->w - text_width(s, scale)) / 2, y, scale, col);
}

/* The panel: title, a field of dots, and one line under it for the error or
   the key hint. No device name, no kernel text: the VT stays in graphics
   mode the whole time. */
static void draw_prompt(canvas_t *c, const layout_t *l, double t) {
    int ps = l->ps, hs = ps > 1 ? ps - 1 : 1;
    fill(c, l->px - ps, l->py - ps, l->pw + 2 * ps, l->ph + 2 * ps, C_SHADOW);
    fill(c, l->px, l->py, l->pw, l->ph, C_PANEL);
    fill(c, l->px, l->py, l->pw, 2 * ps, g_pr.retry && g_pr.st == P_ASK ? C_ERROR : C_BODY);
    int pad = 12 * ps, fx = l->px + pad, fw = l->pw - 2 * pad;
    const char *title = g_pr.title[0] ? g_pr.title : "Enter the disk passphrase";
    int ts = ps;
    /* A long title (the recovery key one) drops a size before it is
       replaced: at the smaller size it still fits most screens. */
    if (text_width(title, ts) > fw && hs < ps && text_width(title, hs) <= fw) ts = hs;
    if (text_width(title, ts) > fw) title = "Enter the disk passphrase", ts = ps;
    draw_center(c, title, l->title_y, ts, C_HOT);

    fill(c, fx, l->field_y, fw, l->field_h, C_FIELD);
    int cy = l->field_y + l->field_h / 2, x = fx + 10 * ps;
    if (g_pr.st == P_ASK && !g_pr.silent) {
        if (g_pr.echo) {
            /* Echo=1: the asker wants the text shown. Non-ASCII as '*'. */
            char vis[128]; size_t k = 0;
            for (size_t i = 0; i < g_pr.len && k + 1 < sizeof vis; i++) {
                unsigned char ch = (unsigned char)g_pr.buf[i];
                if ((ch & 0xC0) == 0x80) continue;
                vis[k++] = ch >= 32 && ch < 127 ? (char)ch : '*';
            }
            vis[k] = 0;
            int maxc = (fw - 20 * ps) / (FONT_W * ps);
            const char *show = (int)k > maxc ? vis + k - maxc : vis;
            draw_text(c, show, x, cy - FONT_H * ps / 2, ps, C_HOT);
            x += text_width(show, ps);
        } else {
            int n = 0;
            for (size_t i = 0; i < g_pr.len; i++)
                if (((unsigned char)g_pr.buf[i] & 0xC0) != 0x80) n++;
            int step = 12 * ps, maxd = (fw - 24 * ps) / step;
            if (n > maxd) n = maxd;
            for (int i = 0; i < n; i++) fill_disc(c, x + 4 * ps + i * step, cy, 4 * ps, C_HOT);
            x += n * step;
        }
        if (((int)(t * 2)) & 1) fill(c, x + 2 * ps, cy - 7 * ps, 2 * ps, 14 * ps, C_CYAN);
    }

    if (g_pr.st == P_CHECKING)
        draw_center(c, "Checking the passphrase...", l->note_y, hs, C_LABEL);
    else if (g_pr.retry)
        draw_center(c, strstr(g_pr.title, "recovery key") ? "Wrong passphrase or recovery key, try again"
                                                          : "Wrong passphrase, try again", l->note_y, hs, C_ERROR);
    else
        draw_center(c, "Enter to unlock \xc2\xb7 Esc to clear", l->note_y, hs, C_LABEL);
}

static layout_t layout(const canvas_t *c) {
    layout_t l;
    /* The wordmark spans about half the width, capped by height, with whole-
       pixel cells so the block art stays crisp. Cells are twice as tall as
       wide, like a console cell. */
    int cw = (c->w / 2) / ART_COLS;
    int byh = (c->h / 4) / (ART_ROWS * 2);
    if (byh < cw) cw = byh;
    if (cw < 2) cw = 2;
    l.cw = cw; l.ch = cw * 2;
    l.ts = c->h >= 1400 ? 3 : c->h >= 900 ? 2 : 1;
    int art_w = ART_COLS * l.cw, art_h = ART_ROWS * l.ch;
    int line = FONT_H * l.ts;
    int block = art_h + line * 4;
    l.ax = (c->w - art_w) / 2;
    l.ay = (c->h - block) / 2 - c->h / 20;          /* a touch above centre */
    l.tag_y = l.ay + art_h + line;
    l.ver_y = l.tag_y + line + line / 3;
    l.sts_y = l.ver_y + line + line / 2;
    l.top = l.ay - l.ch;
    l.bottom = l.sts_y + line + 4 * l.ts;

    /* Panel text: at least 16x32 from 700 rows up, so the question reads at
       a glance; the hint under the field is one step smaller. */
    l.ps = l.ts > 1 ? l.ts : c->h >= 700 ? 2 : 1;
    int ps = l.ps, hs = ps > 1 ? ps - 1 : 1;
    l.pw = 36 * FONT_W * ps;
    if (l.pw > c->w - 32) l.pw = c->w - 32;
    l.px = (c->w - l.pw) / 2;
    l.py = l.tag_y;
    l.title_y = l.py + 14 * ps;
    l.field_y = l.title_y + FONT_H * ps + 10 * ps;
    l.field_h = 22 * ps;
    l.note_y = l.field_y + l.field_h + 9 * ps;
    l.ph = l.note_y + FONT_H * hs + 12 * ps - l.py;
    if (l.py + l.ph + 2 * ps > l.bottom) l.bottom = l.py + l.ph + 2 * ps;
    return l;
}

/* One frame at t seconds. fade is 1 for full brightness, falling to 0 as the
   splash hands the screen over. */
static void gfx_frame(canvas_t *c, const layout_t *l, double t, double fade, int frame) {
    uint32_t rng = (uint32_t)(frame * 2654435761u) ^ 0x9e3779b9u;
    clear_rect(c, 0, l->top, c->w, l->bottom - l->top);

    double gl = t < GLITCH_SEC ? 1.0 - t / GLITCH_SEC : 0.0;
    /* The CRT scanline: a soft bright band sweeping down the wordmark. */
    double period = 1.8, span = ART_ROWS * l->ch;
    double scan_y = fmod(t, period) / period * (span + 6.0 * l->ch) - 3.0 * l->ch;
    int flick = xu(&rng) < 0.04;
    uint32_t bgc = c->bg[(size_t)(l->ay + span / 2) * c->w + c->w / 2];

    /* In the console the ANSI-Shadow face draws its drop shadow with box-drawing
       glyphs. In pixels those turn into a busy grid, so the full blocks are
       drawn twice instead: once offset down-right in the shadow colour, then
       the letter body on top. The box glyphs themselves are skipped. */
    int sx = l->cw / 2 > 0 ? l->cw / 2 : 1, sy = l->ch / 4 > 0 ? l->ch / 4 : 1;
    for (int pass = 0; pass < 2; pass++) {
        uint32_t prng = rng;   /* same glitch decisions in both passes */
        for (int r = 0; r < ART_ROWS; r++) {
            /* Glitch-in: whole rows slip sideways and dissolve, less as gl -> 0. */
            int jitter = gl > 0 && xu(&prng) < gl * .6 ? (int)((xu(&prng) - .5) * gl * 6 * l->cw) : 0;
            const char *p = ART[r]; int col = 0; char g[4];
            while (*p) {
                int a = utf8_next(p, g); if (!a) break;
                int drop = gl > 0 && xu(&prng) < gl * .35;
                int spark = gl > 0 && xu(&prng) < gl * .25;
                if (!drop && !strcmp(g, "█")) {
                    int x = l->ax + col * l->cw + jitter, y = l->ay + r * l->ch;
                    double cy = r * l->ch + l->ch / 2.0 - scan_y;
                    double hot = exp(-(cy * cy) / (2.0 * (l->ch * .9) * (l->ch * .9)));
                    if (pass == 0) {
                        fill(c, x + sx, y + sy, l->cw, l->ch, mix(bgc, mix(C_SHADOW, C_DIM, hot * .5), fade));
                    } else {
                        uint32_t body = mix(C_BODY, hot > .55 ? C_HOT : C_CYAN, hot);
                        if (spark) body = C_HOT;
                        if (flick) body = mix(body, C_BODY, .5);
                        fill(c, x, y, l->cw, l->ch, mix(bgc, body, fade));
                    }
                }
                p += a; col++;
            }
        }
    }
    rng = rng * 1664525u + 1013904223u;

    if (g_pr.st != P_NONE) { draw_prompt(c, l, t); return; }

    int tw = text_width(TAG, l->ts);
    draw_text(c, TAG, (c->w - tw) / 2, l->tag_y, l->ts, mix(bgc, C_TEXT, fade * (gl > 0 ? 1 - gl : 1)));
    if (g_ver[0]) {
        int vw = text_width(g_ver, l->ts);
        draw_text(c, g_ver, (c->w - vw) / 2, l->ver_y, l->ts, mix(bgc, C_DIM, fade));
    }

    /* The typed status line, as in the text renderer. */
    if (t > GLITCH_SEC * .5) {
        double bt = t - GLITCH_SEC * .5;
        int bi = (int)(bt / 1.40); if (bi > 2) bi = 2;
        const char *msg = BOOT[bi];
        int tot = (int)strlen(msg), shown = (int)((bt - bi * 1.40) * 24.0);
        if (shown > tot) shown = tot;
        if (shown < 0) shown = 0;
        char buf[64]; snprintf(buf, sizeof buf, "> %.*s", shown, msg);
        int x = (c->w - text_width("> ", l->ts) - tot * FONT_W * l->ts) / 2;
        draw_text(c, buf, x, l->sts_y, l->ts, mix(bgc, C_DIM, fade));
        if (((int)(t * 2)) & 1)
            fill(c, x + text_width(buf, l->ts), l->sts_y + (FONT_H - 3) * l->ts,
                 FONT_W * l->ts, 2 * l->ts, mix(bgc, C_CYAN, fade));
    }
}

static int g_tty = -1;     /* the VT we switched to graphics mode, if any */
static void tty_text(void) {
    if (g_tty >= 0) { ioctl(g_tty, KDSETMODE, KD_TEXT); g_tty = -1; }
}

/* Returns 0 when done, -1 when there is no usable framebuffer (the caller then
   falls back to the text renderer). */
static int gfx_run(double duration) {
    canvas_t c;
    if (fb_open(&c)) return -1;
    /* Stop fbcon drawing over us. On exit (and from systemd's TTYReset) the VT
       goes back to text mode, so a later console message is never hidden. */
    if (ioctl(STDIN_FILENO, KDSETMODE, KD_GRAPHICS) == 0) { g_tty = STDIN_FILENO; atexit(tty_text); }

    layout_t l = layout(&c);
    blit(&c, 0, c.h);
    agent_register();
    notify_ready();
    const double fade_s = .35;
    double t0 = timeline_start(), started = now_sec();
    for (int frame = 0; !g_exit; frame++) {
        double now = now_sec(), t = now - t0, run = now - started, fade = 1;
        if (duration > 0) {
            if (run >= duration) break;
            if (run > duration - fade_s) fade = (duration - run) / fade_s;
        }
        if (frame % 15 == 14 && fb_stale(&c)) {
            canvas_free(&c);
            if (fb_open(&c)) { usleep(100000); continue; }  /* mid-swap: retry */
            l = layout(&c);
            blit(&c, 0, c.h);
        }
        if (!c.mem) { usleep(100000); if (fb_open(&c) == 0) { l = layout(&c); blit(&c, 0, c.h); } continue; }
        prompt_tick();
        gfx_frame(&c, &l, t, fade, frame);
        blit(&c, l.top, l.bottom);
        usleep(FRAME_USEC);
    }
    /* Leave the plain background, not a frozen logo, for whatever comes next. */
    if (c.mem) { memcpy(c.back, c.bg, (size_t)c.w * c.h * 4); blit(&c, 0, c.h); }
    canvas_free(&c);
    agent_unregister();
    tty_text();
    return 0;
}

/* --render: draw one frame into a PPM, for the build-time check and for
   previewing the design without booting anything. */
static int render_ppm(const char *path, int w, int h, double t, const char *prompt) {
    canvas_t c; memset(&c, 0, sizeof c);
    c.fd = -1; c.w = w; c.h = h; c.bpp = 32; c.stride = w * 4;
    c.r.offset = 16; c.g.offset = 8; c.b.offset = 0; c.r.length = c.g.length = c.b.length = 8;
    c.len = (size_t)c.stride * h;
    c.mem = malloc(c.len);
    if (!c.mem || canvas_alloc(&c)) return 1;
    layout_t l = layout(&c);
    /* --prompt=ask|retry|checking: the passphrase panel with eight
       characters typed, for previewing it without a boot. */
    if (prompt) {
        g_pr.st = !strcmp(prompt, "checking") ? P_CHECKING : P_ASK;
        g_pr.retry = !strcmp(prompt, "retry");
        snprintf(g_pr.title, sizeof g_pr.title, "Enter the disk passphrase");
        g_pr.len = 8; memset(g_pr.buf, 'x', 8);
    }
    gfx_frame(&c, &l, t, 1, 1);
    FILE *f = fopen(path, "wb");
    if (!f) return 1;
    fprintf(f, "P6\n%d %d\n255\n", w, h);
    for (size_t i = 0; i < (size_t)w * h; i++) {
        uint32_t p = c.back[i];
        unsigned char rgb[3] = { (unsigned char)(p >> 16), (unsigned char)(p >> 8), (unsigned char)p };
        fwrite(rgb, 1, 3, f);
    }
    fclose(f);
    free(c.mem); free(c.back); free(c.bg);
    return 0;
}

/* ======================================================================== */

/* "3s", "3", "1.5", "500ms" -> seconds. 0 means "until SIGTERM". */
static double parse_duration(const char *s) {
    char *end; double v = strtod(s, &end);
    if (end == s || v < 0) return 0;
    if (!strcmp(end, "ms")) return v / 1000.0;
    return v;
}

/* A whole word on the kernel command line. */
static int cmdline_has(const char *word) {
    FILE *f = fopen("/proc/cmdline", "r");
    if (!f) return 0;
    char b[4096]; size_t n = fread(b, 1, sizeof b - 1, f); fclose(f);
    b[n] = 0;
    for (char *w = strtok(b, " \t\n"); w; w = strtok(NULL, " \t\n"))
        if (!strcmp(w, word)) return 1;
    return 0;
}

/* The kill switch kairos-init documents: kairos.splash=0 anywhere on the
   kernel command line. */
static int splash_disabled(void) { return cmdline_has("kairos.splash=0"); }

int main(int argc, char **argv) {
    double duration = 0, at = 1.5;
    const char *render = NULL, *prompt = NULL; int rw = 1280, rh = 800;
    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        if (!strncmp(a, "--duration=", 11)) duration = parse_duration(a + 11);
        else if (!strcmp(a, "--duration") && i + 1 < argc) duration = parse_duration(argv[++i]);
        else if (!strncmp(a, "--render=", 9)) render = a + 9;
        else if (!strncmp(a, "--size=", 7)) sscanf(a + 7, "%dx%d", &rw, &rh);
        else if (!strncmp(a, "--at=", 5)) at = parse_duration(a + 5);
        else if (!strncmp(a, "--prompt=", 9)) prompt = a + 9;
        else if (!strcmp(a, "--agent")) g_agent_mode = 1;
        else if (!strcmp(a, "--no-agent")) g_agent_mode = 0;
        /* Anything else is ignored on purpose: a newer kairos-init may pass a
           flag this build does not know, and a splash must never fail a boot
           over that. */
    }
    load_version();
    if (render) return render_ppm(render, rw, rh, at, prompt);
    if (splash_disabled()) { notify_ready(); return 0; }

    if (!isatty(STDOUT_FILENO)) {
        notify_ready();
        if (g_ver[0]) printf("HADRON %s\n", g_ver); else puts("HADRON");
        return 0;
    }

    struct sigaction sa; memset(&sa, 0, sizeof sa); sigemptyset(&sa.sa_mask);
    sa.sa_handler = on_sig;
    sigaction(SIGINT, &sa, NULL); sigaction(SIGTERM, &sa, NULL);

    if (gfx_run(duration) == 0) return 0;
    /* No framebuffer: no agent. The console agent asks instead. */
    notify_ready();
    return text_run(duration);
}
