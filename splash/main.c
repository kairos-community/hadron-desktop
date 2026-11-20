/* isotope-splash — animated isotope boot splash, installed as
 * /usr/bin/kairos-splash so Kairos runs it in both halves of the boot.
 *
 * The brand direction "One light": the core of the mark is the only light on
 * the screen, the shell takes that light, and the sixth side, lifted off, lets
 * it out. The text renderer further down draws the wordmark only.
 *
 * Two renderers:
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
 * In the initramfs, D (or isotope.bootvine on the command line) hands the
 * screen to bootvine, which shows the boot's systemd jobs as they run.
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
#include <linux/vt.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <errno.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>

#include "agent.h"
#include "masks.h"

#define FRAME_USEC   33333
#define MIN_COLS     58
#define MIN_ROWS     12
#define GLITCH_SEC   1.10   /* logo resolves out of glitch over this window */

/* The Isotope ramp, dim -> hot. The kernel VT/framebuffer is a 16-color VGA
   console, so we use its palette indices, which the kernel command line sets
   from theme/palette.toml (tools/gen-theme.py, VT_ROLES): 4 and 12 = accent
   (lime), 8 = dim, 14 = glow (light lime), 15 = text, white-hot. */
static const uint8_t RAMP[8] = {4, 4, 8, 12, 12, 12, 14, 15};
#define A_CHROME 1   /* faint HUD brackets / leaders */
#define A_DIM    2   /* tagline, version */
#define A_BODY   4   /* logo at rest */
#define A_BRIGHT 6   /* scanline shoulder */
#define A_HOT    7   /* scanline crest, glitch sparks, cursor */

static char g_ver[64] = "";

/* The isotope release, written by the Dockerfile into the branding
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
#define ART_COLS 54
static const char *ART[ART_ROWS] = {
"██╗███████╗ ██████╗ ████████╗ ██████╗ ██████╗ ███████╗",
"██║██╔════╝██╔═══██╗╚══██╔══╝██╔═══██╗██╔══██╗██╔════╝",
"██║███████╗██║   ██║   ██║   ██║   ██║██████╔╝█████╗  ",
"██║╚════██║██║   ██║   ██║   ██║   ██║██╔═══╝ ██╔══╝  ",
"██║███████║╚██████╔╝   ██║   ╚██████╔╝██║     ███████╗",
"╚═╝╚══════╝ ╚═════╝    ╚═╝    ╚═════╝ ╚═╝     ╚══════╝",
};
static const char *TAG = "immutable Linux \u00b7 built on Hadron, by Kairos";
/* In-progress, honest boot phrases — true during early boot, never claiming a
   service is "ready" before it is. */
static const char *BOOT[3] = {
    "waking the machine",
    "mounting the immutable core",
    "bringing up the desktop",
};

static uint32_t xs(uint32_t *s) { uint32_t x = *s; x ^= x<<13; x ^= x>>17; x ^= x<<5; *s = x?x:0xdeadbeef; return *s; }
static double   xu(uint32_t *s) { return (xs(s) & 0xFFFFFF) / (double)0x1000000; }

/* paint_logo: the ISOTOPE wordmark with a downward CRT scanline (crest = white-
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
                g_set(g, top + r, left + cc, put, RAMP[ci]);
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
#define TIMELINE "/run/isotope-splash.start"
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

/* The D key and bootvine, defined with the password agent below. */
static int g_hotkey, g_want_bootvine;
static int hotkey_offer(void);
static void hotkey_read(void);
static int bootvine_start(void);
static void kb_restore(void);
static void tty_text(void);

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

    /* The text renderer is never a password agent: the console agent's gate
       stops it before the console asks, so reading tty1 here takes no
       passphrase keys. */
    g_hotkey = hotkey_offer();
    static const char *HINT = "Press D for boot details";

    double t0 = now_sec();
    int frame = 0;
    while (!g_exit) {
        double tn = now_sec(), t = tn - t0;
        if (duration > 0 && t >= duration) break;
        hotkey_read();
        if (g_want_bootvine) {
            g_want_bootvine = 0;
            if (bootvine_start() == 0) break;
            g_hotkey = 0;
        }

        uint32_t rng = (uint32_t)(frame * 2654435761u) ^ 0x9e3779b9u;
        g_clear(&cur);

        int flick = (xu(&rng) < 0.05) ? 1 : 0;                 /* rare CRT flicker */
        int scan  = ((int)(t * 9.0)) % (ART_ROWS + 4) - 2;     /* sweep down, wrap */
        double gl = t < GLITCH_SEC ? (1.0 - t / GLITCH_SEC) : 0.0;

        paint_brackets(&cur, fr_t, fr_l, fr_b, fr_r, RAMP[A_CHROME]);
        paint_logo(&cur, top, left, scan, flick, gl, &rng);
        paint_center(&cur, tag_row, TAG, RAMP[flick ? A_CHROME : A_DIM]);
        if (g_ver[0]) paint_center(&cur, ver_row, g_ver, RAMP[A_CHROME]);
        if (g_hotkey && rows - 2 > fr_b) paint_center(&cur, rows - 2, HINT, RAMP[A_DIM]);

        /* typewritten boot line, cycling through the phrases, blinking cursor */
        if (t > GLITCH_SEC * 0.5) {
            double bt = t - GLITCH_SEC * 0.5;
            int bi = (int)(bt / 1.40); if (bi > 2) bi = 2;
            const char *msg = BOOT[bi];
            int shown = (int)((bt - bi * 1.40) * 24.0);
            int tot = (int)strlen(msg); if (shown > tot) shown = tot; if (shown < 0) shown = 0;
            int sl = (cols - (tot + 4)) / 2; if (sl < 0) sl = 0;
            g_set(&cur, sts_row, sl, ">", RAMP[A_DIM]);
            for (int i = 0; i < shown; i++) { char ch[2] = { msg[i], 0 }; g_set(&cur, sts_row, sl + 2 + i, ch, RAMP[A_BODY]); }
            if (((int)(t * 2)) & 1) g_set(&cur, sts_row, sl + 2 + shown, "_", RAMP[A_HOT]);
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
    kb_restore();
    return 0;
}

/* ======================================================================== */
/* Graphics: draw into a framebuffer.                                        */
/* ======================================================================== */

/* Brand: Autunite. The yellow is used as light, so most of what is drawn in
   it is a share of these, added to the ground. Theme greys: Carbon night. */
#define C_CRUST    0x0c0d0b
#define C_BASE     0x141411
#define C_SURFACE  0x1d1d1a
#define C_OVERLAY  0x2a2b25
#define C_BORDER   0x393a34
#define C_MUTED    0x595b52
#define C_LABEL    0x909287
#define C_SUBTEXT  0xced0c8
#define C_HOT      0xf4f5f2
#define C_BRAND    0xe4de48
#define C_BGLOW    0xf9f1af
#define C_BDEEP    0x9f9615
#define C_ERROR    0xff7172
#define C_WARN     0xea8a18

/* A draw target: the mmapped framebuffer, or a heap buffer for --render.
   Pixels are composed in an XRGB back buffer and packed into the target's
   own layout (32 or 16 bpp, any channel offsets) when blitted. */
typedef struct {
    int fd;
    uint8_t *mem; size_t len;
    int w, h, stride, bpp;
    struct fb_bitfield r, g, b;
    uint32_t *back;   /* w*h XRGB */
    uint32_t *bg;     /* w*h XRGB: the ground with the light on it */
    float *lm;        /* the light, on every fourth pixel */
    int lit; double lit_power, lit_lift;   /* what bg was painted for */
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

/* Until the first frame paints the scene, the ground is the plain crust. */
static void paint_background(canvas_t *c) {
    for (size_t i = 0; i < (size_t)c->w * c->h; i++) c->bg[i] = C_CRUST;
    c->lit = 0;
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
    free(c->back); free(c->bg); free(c->lm);
    c->back = c->bg = NULL; c->lm = NULL; c->lit = 0;
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

/* Text: pre-rendered alpha masks from masks.h, in three sizes: g_tier 0 under
   900 rows, 1 (1.3 times) under 1500, 2 (double) above. */
static int g_tier;

static void blit_mask(canvas_t *c, const unsigned char *d, int n, int w, int x0, int y0, uint32_t col, double a) {
    int i = 0;
    for (int k = 0; k < n; k++) {
        int b = d[k], run = b < 0x40 ? b + 1 : b >= 0xc0 ? b - 0xbf : 1;
        double cov = b < 0x40 ? 0 : b >= 0xc0 ? 1 : (b - 0x40) / 127.0;
        if (cov > 0) for (int j = 0; j < run; j++) {
            int x = x0 + (i + j) % w, y = y0 + (i + j) / w;
            if (x < 0 || y < 0 || x >= c->w || y >= c->h) continue;
            uint32_t *px = c->back + (size_t)y * c->w + x;
            *px = mix(*px, col, cov * a);
        }
        i += run;
    }
}

static const face_t *face(int f) { return &FACE[f * 3 + g_tier]; }

/* The next character's glyph. "·" (U+00B7) is in the set; anything else
   outside ASCII is drawn as "?". */
static const glyph_t *next_glyph(const face_t *f, const char **s) {
    unsigned char ch = (unsigned char)**s; (*s)++;
    if (ch == 0xc2 && (unsigned char)**s == 0xb7) { (*s)++; ch = 0xb7; }
    else if (ch >= 0x80) { while (((unsigned char)**s & 0xc0) == 0x80) (*s)++; ch = '?'; }
    const char *p = strchr(f->set, (char)ch);
    if (!p) p = strchr(f->set, '?');
    if (!p) p = f->set;
    return &f->g[p - f->set];
}

static int text_width(int fi, const char *s, int track) {
    const face_t *f = face(fi); int n = 0;
    while (*s) n += next_glyph(f, &s)->adv + track;
    return n - track;
}

/* x is the left edge, y the baseline. */
static void draw_text(canvas_t *c, int fi, const char *s, int x, int y, int track, uint32_t col, double a) {
    const face_t *f = face(fi);
    while (*s) {
        const glyph_t *g = next_glyph(f, &s);
        if (g->n) blit_mask(c, f->d + g->off, g->n, g->w, x + g->xo, y + g->yo, col, a);
        x += g->adv + track;
    }
}

static void draw_center(canvas_t *c, int fi, const char *s, int y, int track, uint32_t col, double a) {
    draw_text(c, fi, s, (c->w - text_width(fi, s, track)) / 2, y, track, col, a);
}

typedef struct {
    double u;            /* one design pixel: the design is drawn for 1080 rows */
    int tu;              /* text unit: 1, or 2 on the double size */
    int trk;             /* letter spacing of the small capitals */
    double mx, my, mr;   /* the mark: the core's centre and the hexagon's corner radius */
    const wordmark_t *wm;
    int base_y;          /* the wordmark's baseline */
    int seg_w, seg_gap, seg_h, seg_y, lab_y, ver_y, foot_y;
    int title_y, field_w, field_h, field_y, note_y, pver_y;
    int top, bottom;     /* rows that change while the light stands still */
} layout_t;

/* ======================================================================== */
/* Password agent: the disk passphrase prompt.                               */
/* ======================================================================== */

/* While the initramfs splash answers systemd's password questions it says so
   in this file (its PID), and the console agent's gate waits for that PID
   instead of asking on the console too. See
   rootfs/usr/lib/dracut/modules.d/51isotope-askpass/. */
#define AGENT_PIDDIR  "/run/isotope-ask"
#define AGENT_PIDFILE AGENT_PIDDIR "/splash-agent"

#define CHECKING_SEC 4.0   /* "Checking..." after Enter, until a retry comes */

typedef enum { P_NONE, P_ASK, P_CHECKING } pstate_t;

/* The story's clock, and when on it the last key was typed. */
static double g_clock, g_key_at = -9;

static struct {
    int on;                    /* registered as an agent */
    agent_t ag;
    pstate_t st;
    double checking_until;
    char title[64];
    int retry, echo, silent;
    char buf[512]; size_t len;
} g_pr;

/* tty1 in raw mode: one byte per key, no echo, no line editing, no signals
   from Ctrl-C. The kernel keymap still applies, so a German or Italian layout
   loaded by systemd-vconsole-setup types what its keys say. Both the
   passphrase prompt and the D key read it. */
static struct { int raw, hooked; struct termios saved; } g_kb;

static void kb_restore(void) {
    if (g_kb.raw) tcsetattr(STDIN_FILENO, TCSANOW, &g_kb.saved);
    g_kb.raw = 0;
}

static int kb_raw(void) {
    if (g_kb.raw) return 1;
    if (!isatty(STDIN_FILENO) || tcgetattr(STDIN_FILENO, &g_kb.saved)) return 0;
    struct termios t = g_kb.saved;
    t.c_lflag &= ~(tcflag_t)(ICANON | ECHO | ECHONL | ISIG | IEXTEN);
    t.c_iflag &= ~(tcflag_t)(IXON | ICRNL | INLCR | IGNCR | ISTRIP);
    t.c_cc[VMIN] = 0; t.c_cc[VTIME] = 0;
    if (tcsetattr(STDIN_FILENO, TCSANOW, &t)) return 0;
    g_kb.raw = 1;
    if (!g_kb.hooked) { g_kb.hooked = 1; atexit(kb_restore); }
    return 1;
}

/* ------------------------------------------------------------------------ */
/* bootvine (codeberg.org/wvhulle/bootvine): a live tree of the boot's systemd
   jobs, the units that failed and why, and the slowest ones, on tty12. In the
   initramfs D hands the screen to it; isotope.bootvine on the kernel command
   line does so at once. bootvine.service is installed but not enabled (the
   52isotope-bootvine dracut module, the Dockerfile `bootvine` stage), so it
   runs only when the splash starts it. It survives switch-root by itself and
   follows the rest of the boot; the marker keeps the booted-system splash
   from drawing over it. It is also a password agent, and the console agent's
   gate (51isotope-askpass) waits while it runs. */
#define BOOTVINE_BIN  "/usr/bin/bootvine"
#define BOOTVINE_UNIT "bootvine.service"
#define BOOTVINE_MARK "/run/isotope-bootvine"

/* g_hotkey: D is offered and the hint is shown. g_want_bootvine: D was
   pressed. Both are declared above text_run. */

static int bootvine_installed(void) {
    return access(BOOTVINE_BIN, X_OK) == 0 && access("/run/systemd/system", F_OK) == 0;
}

/* systemctl start, waiting at most 5 s: bootvine.service has no ordering
   that could hold its job, so the start takes a moment. The marker goes first,
   so a booted-system splash starting meanwhile already stays out. 0 = it
   runs. */
static int bootvine_start(void) {
    int fd = open(BOOTVINE_MARK, O_WRONLY | O_CREAT | O_CLOEXEC, 0644);
    if (fd >= 0) close(fd);
    pid_t pid = fork();
    if (pid == 0) {
        int nul = open("/dev/null", O_RDWR);
        if (nul >= 0) { dup2(nul, STDIN_FILENO); dup2(nul, STDOUT_FILENO); }
        execlp("systemctl", "systemctl", "start", BOOTVINE_UNIT, (char *)NULL);
        _exit(127);
    }
    /* The console in front now, tty1. */
    struct vt_stat vs; int fg = ioctl(STDIN_FILENO, VT_GETSTATE, &vs) == 0 ? vs.v_active : -1;
    int st = -1;
    if (pid > 0) {
        double until = now_sec() + 5;
        for (;;) {
            pid_t r = waitpid(pid, &st, WNOHANG);
            if (r == pid) break;
            if (r < 0 && errno != EINTR) { st = -1; break; }
            if (now_sec() > until) { st = -1; break; }   /* leave it running */
            usleep(20000);
        }
    }
    if (st == 0) {
        /* The unit is active once its process runs, a moment before bootvine
           has connected to systemd and switched to tty12. The kernel does not
           complete that switch while tty1 is in graphics mode, so tty1 goes
           back to text mode first, cleared and with no cursor: the moment in
           between is an empty screen, not the kernel messages fbcon would
           redraw. Then wait for the switch, 3 s at most. */
        fputs("\033[?25l\033[H\033[2J\033[3J", stdout);
        fflush(stdout);
        tty_text();
        double until = now_sec() + 3;
        while (fg > 0 && now_sec() < until &&
               ioctl(STDIN_FILENO, VT_GETSTATE, &vs) == 0 && vs.v_active == fg)
            usleep(20000);
        return 0;
    }
    fprintf(stderr, "kairos-splash: could not start %s\n", BOOTVINE_UNIT);
    unlink(BOOTVINE_MARK);
    return -1;
}

/* D only in the initramfs: the booted-system splash lasts three seconds and
   the login screen takes tty1 back right after it. Puts tty1 in raw mode. */
static int hotkey_offer(void) {
    return !access("/etc/initrd-release", F_OK) && bootvine_installed() &&
           access(BOOTVINE_MARK, F_OK) && kb_raw();
}

static void hotkey_keys(const unsigned char *b, size_t n);

/* Without a passphrase question (the text renderer never answers one), read
   whatever was typed and look for D. */
static void hotkey_read(void) {
    if (!g_hotkey || !g_kb.raw) return;
    unsigned char b[64]; ssize_t n;
    while ((n = read(STDIN_FILENO, b, sizeof b)) > 0) hotkey_keys(b, (size_t)n);
}

static int cmdline_has(const char *word);

static void prompt_wipe(void) {
    volatile char *p = g_pr.buf;
    for (size_t i = 0; i < sizeof g_pr.buf; i++) p[i] = 0;
    g_pr.len = 0;
}

static void agent_unregister(void) {
    if (!g_pr.on) return;
    unlink(AGENT_PIDFILE);
    agent_close(&g_pr.ag);
    prompt_wipe();
    g_pr.on = 0;
}

/* 1 = always (--agent), 0 = never (--no-agent, isotope.splash.agent=0),
   -1 = only in the initramfs. */
static int g_agent_mode = -1;

/* Answer questions only when the screen can show them and the keyboard can
   reach us: a framebuffer (the caller has one), the VT on stdin, and the
   right to write to the asker's socket. In the booted system the stock
   console agent asks, so the 3-second splash there stays out of it unless
   --agent says otherwise. */
static void agent_register(void) {
    /* hadron.splash.agent=0 is the name from before the rename to Isotope. */
    if (g_agent_mode == 0 || cmdline_has("isotope.splash.agent=0") ||
        cmdline_has("hadron.splash.agent=0")) return;
    if (g_agent_mode < 0 && access("/etc/initrd-release", F_OK)) return;
    if (!isatty(STDIN_FILENO)) return;
    if (!ask_can_reply()) return;

    mkdir(AGENT_PIDDIR, 0755);
    char tmp[64];
    snprintf(tmp, sizeof tmp, "%s.%d", AGENT_PIDFILE, (int)getpid());
    FILE *f = fopen(tmp, "w");
    if (!f) return;
    fprintf(f, "%d\n", (int)getpid());
    if (fclose(f) || rename(tmp, AGENT_PIDFILE)) { unlink(tmp); return; }

    kb_raw();
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
            g_key_at = g_clock;
        }
    }
}

/* Keys while no question is open: D (either case) asks for bootvine.
   Escape sequences are skipped whole, so the left arrow (ESC [ D) is not a
   D. */
static void hotkey_keys(const unsigned char *b, size_t n) {
    for (size_t i = 0; i < n; i++) {
        if (b[i] == 0x1b && i + 1 < n) {
            i++;
            if (b[i] == '[' || b[i] == 'O') {
                while (i + 1 < n && !(b[i + 1] >= 0x40 && b[i + 1] <= 0x7e)) i++;
                if (i + 1 < n) i++;
            }
        } else if (b[i] == 'd' || b[i] == 'D') {
            g_want_bootvine = 1;
        }
    }
}

/* One step per frame: watch the directory, read the keyboard. */
static void prompt_tick(void) {
    if (!g_pr.on) { hotkey_read(); return; }
    double now = now_sec();
    int r = agent_poll(&g_pr.ag, now);
    if (r == 1) prompt_show();
    else if (r == 2 && g_pr.st == P_ASK) g_pr.st = P_NONE;
    if (g_pr.st == P_CHECKING && now > g_pr.checking_until) g_pr.st = P_NONE;
    if (g_pr.st != P_ASK) hotkey_read();
    if (g_pr.st == P_ASK) {
        unsigned char b[64];
        ssize_t n;
        while ((n = read(STDIN_FILENO, b, sizeof b)) > 0) prompt_keys(b, (size_t)n);
        memset(b, 0, sizeof b);
    }
}

/* ------------------------------------------------------------------------ */
/* The scene: one light.

   The core of the mark is the only light on the screen. The shell is a body
   that takes that light on its inner faces; the sixth side, lifted off, is
   where the light gets out: two soft shafts leave along the hexagon's top and
   upper right corners, past the two ends of the lifted side. Nothing here is
   a picture: it is the mark's own geometry (tools/geom.py: mark M1), evaluated
   per pixel, so it is the same at any size.

   Mark units: the hexagon's corner radius is 1, y points up, the core is at
   0,0. Shell thickness 0.150, lift 0.200, core radius 0.170. */
#define K30     0.8660254037844386
#define M_T     0.150
#define M_LIFT  0.200
#define M_CORE  0.170
#define M_AIN   (K30 - M_T)        /* centre to the shell's inner faces */
#define SHAFT_GAIN   0.36   /* a shaft's light where it leaves the slit */
#define SHAFT_SPREAD 0.115  /* how fast it widens: the core seen through the slit */
#define SHAFT_THIN   0.55   /* how fast it thins as it widens */
#define SHAFT_REACH  6.0    /* and how far it carries, in mark radii */

typedef struct {
    double power;    /* 0..1: how bright the core burns */
    double shell;    /* 0..1: how far the shell has taken the light (0: a dark body with a lit rim) */
    double lift;     /* 0..1: the sixth side, from seated to fully lifted */
    double core;     /* 0..1: the core's size as it ignites; 0 draws no core */
    double flash;    /* 0..1: the lifted side as it lets go */
    /* derived by scene_derive() */
    double sx[2], sy[2], ax[2], ay[2], w0, sh;
} scene_t;

static double clamp01(double v) { return v < 0 ? 0 : v > 1 ? 1 : v; }
static double sstep(double a, double b, double v) { v = clamp01((v - a) / (b - a)); return v * v * (3 - 2 * v); }
static double seg(double t, double start, double len) { return clamp01((t - start) / len); }
static double ease_out(double p) { p = 1 - p; return 1 - p * p * p; }
/* Overshoots a little past 1, then settles. */
static double ease_back(double p) { double k = 1.4; p -= 1; return 1 + p * p * ((k + 1) * p + k); }

/* The two slits. Each is the gap between an end of the shell and an end of
   the lifted side: one at the top corner, one at the upper right corner,
   mirror images across the lifted side's normal. The core is wider than a
   slit, so what leaves is the core's image through a pinhole: a soft cone
   that starts as narrow as the slit and thins out as it spreads. */
static void scene_derive(scene_t *s) {
    double lf = s->lift * M_LIFT, x = lf * .25, y = 1.0 + lf * .4, n = sqrt(x * x + y * y);
    s->sx[0] = x; s->sy[0] = y; s->ax[0] = x / n; s->ay[0] = y / n;
    /* mirrored across the line at 60 degrees */
    s->sx[1] = -.5 * x + K30 * y; s->sy[1] = K30 * x + .5 * y;
    s->ax[1] = s->sx[1] / n; s->ay[1] = s->sy[1] / n;
    s->w0 = lf * .25 + .004;
    s->sh = s->power * s->lift * sqrt(s->lift);
}

/* Distance from the centre to the hexagon side a point belongs to. */
static double hexd(double x, double y) {
    double a = fabs(x), b = fabs(.5 * x + K30 * y), c = fabs(-.5 * x + K30 * y);
    return a > b ? (a > c ? a : c) : (b > c ? b : c);
}

/* The amount of the core's light in the air at a point, 0..1 and a little
   more next to the core. */
static double light_at(const scene_t *s, double x, double y) {
    double d2 = x * x + y * y, h = hexd(x, y);
    double sect = fmin(x, -.5 * x + K30 * y);   /* > 0 in the lifted side's sixth */
    double lf = s->lift * M_LIFT;
    /* Inside the shell the light is held; under the lifted side it reaches
       as far as that side now is. */
    double hb = M_AIN + M_T * .5 + lf * sstep(-.01, .02, sect);
    double in = 1 - sstep(hb - M_T * .5, hb + M_T * .5, h);
    double I = in > 0 ? in * s->power * (.46 * exp(-d2 / (.30 * .30)) + .15 * exp(-d2 / (.60 * .60)) + .030) : 0;
    double out = sstep(M_AIN, 1.0, h);
    /* A little light in the air around the mark, so it stands in a room. */
    I += out * s->power * .026 / (1 + d2 / (2.4 * 2.4));
    if (s->sh > 0) for (int k = 0; k < 2; k++) {
        double vx = x - s->sx[k], vy = y - s->sy[k];
        double along = vx * s->ax[k] + vy * s->ay[k], across = fabs(vx * s->ay[k] - vy * s->ax[k]);
        if (along <= -.27) continue;   /* the slit itself is lit: the light is on its way through */
        double wd = s->w0 + (along > 0 ? along : 0) * SHAFT_SPREAD;
        double e = across / wd;
        if (e > 3) continue;
        I += s->sh * SHAFT_GAIN * sstep(-.27, -.13, along) * exp(-e * e) * pow(s->w0 / wd, SHAFT_THIN)
             / (1 + along * along / (SHAFT_REACH * SHAFT_REACH));
    }
    return I;
}

/* The ground: Carbon base around the mark falling to crust at the edges. */
static long g_ground = -1;   /* --ground=rrggbb: one flat colour, for a ground inside a window */
static void ground_at(const canvas_t *c, const layout_t *l, int x, int y, double *rgb) {
    if (g_ground >= 0) { rgb[0] = g_ground >> 16 & 255; rgb[1] = g_ground >> 8 & 255; rgb[2] = g_ground & 255; return; }
    double dx = (x - l->mx) / c->h, dy = (y - l->my) / c->h;
    double k = sstep(.10, 1.05, sqrt(dx * dx + dy * dy));
    rgb[0] = 0x14 + (0x0c - 0x14) * k; rgb[1] = 0x14 + (0x0d - 0x14) * k; rgb[2] = 0x11 + (0x0b - 0x11) * k;
}

/* Add light of strength I. Faint, it is the brand yellow a touch warmer;
   strong, it goes to the brand's light tone. */
static void add_light(double *rgb, double I) {
    double hot = sstep(.30, 1.0, I);
    rgb[0] += 255 * I * 1.00;
    rgb[1] += 255 * I * (.885 + .08 * hot);
    rgb[2] += 255 * I * (.30 + .42 * hot);
}

/* Round to 8 bits with a little noise, so the dark gradients do not band. */
static uint32_t quantise(const double *rgb, int x, int y) {
    uint32_t h = (uint32_t)x * 374761393u + (uint32_t)y * 668265263u;
    h = (h ^ (h >> 13)) * 1274126177u; h ^= h >> 16;
    double n = ((h & 1023) + ((h >> 10) & 1023)) / 1023.0 - 1.0;   /* triangular, -1..1 */
    uint32_t out = 0;
    for (int i = 0; i < 3; i++) {
        double v = rgb[i] + n * .5 + .5;
        out = out << 8 | (uint32_t)(v < 0 ? 0 : v > 255 ? 255 : v);
    }
    return out;
}

/* Paint the ground and the light into c->bg. Both are smooth away from the
   mark, so the colour is worked out on every fourth pixel and stretched; the
   mark's own square is drawn exactly by draw_mark. This runs for every frame
   while the light is changing, so the inner loop is three additions, a
   little noise against banding, and a clamp. */
static float g_noise[64 * 64];
static void paint_scene(canvas_t *c, const layout_t *l, const scene_t *s) {
    /* The grid follows the mark's size, so the work is the same on any screen. */
    int LGRID = l->mr >= 220 ? 8 : l->mr >= 100 ? 4 : 3;
    int lw = c->w / LGRID + 2, lh = c->h / LGRID + 2;
    if (!c->lm) c->lm = malloc((size_t)lw * (lh + 1) * 3 * sizeof(float));
    if (!c->lm) return;
    if (g_noise[0] == 0) for (int i = 0; i < 64 * 64; i++) {
        uint32_t h = (uint32_t)i * 2654435761u; h = (h ^ (h >> 13)) * 1274126177u; h ^= h >> 16;
        g_noise[i] = (float)(((h & 1023) + ((h >> 10) & 1023)) / 1023.0 * .5 + 1e-6);   /* triangular, 0..1 */
    }
    for (int j = 0; j < lh; j++)
        for (int i = 0; i < lw; i++) {
            double rgb[3];
            ground_at(c, l, i * LGRID, j * LGRID, rgb);
            add_light(rgb, light_at(s, (i * LGRID - l->mx) / l->mr, -(j * LGRID - l->my) / l->mr));
            float *n = c->lm + ((size_t)j * lw + i) * 3;
            n[0] = (float)rgb[0]; n[1] = (float)rgb[1]; n[2] = (float)rgb[2];
        }
    float *row = c->lm + (size_t)lh * lw * 3;     /* one stretched row of nodes */
    for (int y = 0; y < c->h; y++) {
        float fy = (float)(y % LGRID) / LGRID;
        const float *r0 = c->lm + (size_t)(y / LGRID) * lw * 3, *r1 = r0 + lw * 3;
        for (int i = 0; i < lw * 3; i++) row[i] = r0[i] + (r1[i] - r0[i]) * fy;
        uint32_t *out = c->bg + (size_t)y * c->w;
        const float *nz = g_noise + (y * 29 & 63) * 64;
        int x = 0;
        for (int i = 0; x < c->w; i++) {
            const float *a = row + i * 3;
            float r = a[0], g = a[1], b = a[2];
            float dr = (a[3] - r) / LGRID, dg = (a[4] - g) / LGRID, db = (a[5] - b) / LGRID;
            for (int k = 0; k < LGRID && x < c->w; k++, x++) {
                float n = nz[(x + y * 17) & 63], R = r + n, G = g + n, B = b + n;
                out[x] = (uint32_t)(R > 255 ? 255 : R) << 16 | (uint32_t)(G > 255 ? 255 : G) << 8 | (uint32_t)(B > 255 ? 255 : B);
                r += dr; g += dg; b += db;
            }
        }
    }
}

static void over(double *rgb, uint32_t col, double a) {
    rgb[0] += ((col >> 16 & 255) - rgb[0]) * a;
    rgb[1] += ((col >> 8 & 255) - rgb[1]) * a;
    rgb[2] += ((col & 255) - rgb[2]) * a;
}

/* The colour of the shell (lid = 0) or of the lifted side (lid = 1) at u,
   0 on the face towards the core, 1 on the outside. Unlit it is a graphite
   body with the core's light on its inner edge; as it takes the light the
   brand colour spreads from that edge outwards. The lifted side faces the
   core, so it is the lighter tone: that is why the flat mark has two. */
static uint32_t body_colour(const scene_t *s, double u, int lid) {
    uint32_t dark = mix(0x23241f, C_BGLOW, clamp01(s->power * (lid ? 1.0 : .85) * exp(-u * 4.2)));
    uint32_t full = lid ? mix(mix(C_BGLOW, 0xffffff, .35), C_BGLOW, sstep(0, .45, u))
                        : mix(mix(C_BRAND, C_BGLOW, .40), C_BRAND, sstep(0, .38, u));
    uint32_t col = mix(dark, full, clamp01(s->shell * 1.7 - u * .7));
    return lid ? mix(col, 0xffffff, s->flash * .7) : col;
}

/* The mark, exactly, over its own square of the screen. */
static void draw_mark(canvas_t *c, const layout_t *l, const scene_t *s, double fade) {
    double px = 1.0 / l->mr, lf = s->lift * M_LIFT, cr = M_CORE * (.30 + .70 * s->core);
    int x0 = (int)(l->mx - 1.7 * l->mr), x1 = (int)(l->mx + 1.7 * l->mr) + 1;
    int y0 = (int)(l->my - 1.7 * l->mr), y1 = (int)(l->my + 1.7 * l->mr) + 1;
    if (x0 < 0) x0 = 0;
    if (y0 < 0) y0 = 0;
    if (x1 > c->w) x1 = c->w;
    if (y1 > c->h) y1 = c->h;
    for (int py = y0; py < y1; py++) {
        for (int pxl = x0; pxl < x1; pxl++) {
            double x = (pxl + .5 - l->mx) * px, y = -(py + .5 - l->my) * px, rgb[3];
            double h = hexd(x, y), sect = fmin(x, -.5 * x + K30 * y), solid = 0;
            ground_at(c, l, pxl, py, rgb);
            add_light(rgb, light_at(s, x, y) * fade);
            /* shell: the hexagon band, less the lifted side's sixth */
            double sd = fmax(fmax(h - K30, M_AIN - h), sect), cov = clamp01(.5 - sd / px);
            if (cov > 0) { over(rgb, body_colour(s, clamp01((h - M_AIN) / M_T), 0), cov * fade); solid = cov; }
            /* the lifted side: the same sixth, moved out along its normal */
            double x2 = x - .5 * lf, y2 = y - K30 * lf, h2 = .5 * x2 + K30 * y2;
            sd = fmax(fmax(h2 - K30, M_AIN - h2), -fmin(x2, -.5 * x2 + K30 * y2));
            cov = clamp01(.5 - sd / px);
            if (cov > 0) { over(rgb, body_colour(s, clamp01((h2 - M_AIN) / M_T), 1), cov * fade); solid = fmax(solid, cov); }
            if (s->core > 0) {
                double d = sqrt(x * x + y * y);
                cov = clamp01(.5 - (d - cr) / px);
                if (cov > 0) {
                    uint32_t lit = mix(0xfffef4, C_BGLOW, sstep(.35, 1.0, d / cr));
                    over(rgb, mix(0x6a6420, lit, sstep(0, .55, s->power)), cov * fade);
                    solid = fmax(solid, cov);
                }
            }
            uint32_t q = quantise(rgb, pxl, py);
            if (solid >= 1) q = (uint32_t)(rgb[0] + .5) << 16 | (uint32_t)(rgb[1] + .5) << 8 | (uint32_t)(rgb[2] + .5);
            c->back[(size_t)py * c->w + pxl] = q;
        }
    }
}

/* Fill a convex polygon of n points grown by radius r, so its corners are
   round: one point is a disc, two are a round-ended bar. col is blended over
   the back buffer with alpha a; the edge is a one-pixel ramp. */
static void fill_round(canvas_t *c, const double *px, const double *py, int n, double r, uint32_t col, double a) {
    if (a <= 0) return;
    double lx = px[0], hx = px[0], ly = py[0], hy = py[0];
    for (int i = 1; i < n; i++) {
        lx = fmin(lx, px[i]); hx = fmax(hx, px[i]); ly = fmin(ly, py[i]); hy = fmax(hy, py[i]);
    }
    int x0 = (int)floor(lx - r) - 1, x1 = (int)ceil(hx + r) + 1;
    int y0 = (int)floor(ly - r) - 1, y1 = (int)ceil(hy + r) + 1;
    if (x0 < 0) x0 = 0;
    if (y0 < 0) y0 = 0;
    if (x1 > c->w) x1 = c->w;
    if (y1 > c->h) y1 = c->h;
    for (int y = y0; y < y1; y++) {
        uint32_t *row = c->back + (size_t)y * c->w;
        for (int x = x0; x < x1; x++) {
            double fx = x + .5, fy = y + .5, d2 = 1e18;
            for (int i = 0; i < n; i++) {
                int j = (i + 1) % n;
                double ex = px[j] - px[i], ey = py[j] - py[i], wx = fx - px[i], wy = fy - py[i];
                double ll = ex * ex + ey * ey, u = ll > 0 ? (wx * ex + wy * ey) / ll : 0;
                u = u < 0 ? 0 : u > 1 ? 1 : u;
                double dx = wx - u * ex, dy = wy - u * ey, dd = dx * dx + dy * dy;
                if (dd < d2) d2 = dd;
            }
            double cov = .5 - (sqrt(d2) - r);
            if (cov <= 0) continue;
            if (cov > 1) cov = 1;
            row[x] = mix(row[x], col, cov * a);
        }
    }
}

/* A small pool of light on the back buffer: the head of the progress line,
   the dot of the i, a typed character. */
static void glow(canvas_t *c, double cx, double cy, double r, uint32_t col, double a) {
    int x0 = (int)(cx - 3 * r), x1 = (int)(cx + 3 * r) + 1, y0 = (int)(cy - 3 * r), y1 = (int)(cy + 3 * r) + 1;
    if (a <= 0 || r <= 0) return;
    for (int y = y0 < 0 ? 0 : y0; y < y1 && y < c->h; y++)
        for (int x = x0 < 0 ? 0 : x0; x < x1 && x < c->w; x++) {
            double dx = x + .5 - cx, dy = y + .5 - cy, g = exp(-(dx * dx + dy * dy) / (r * r)) * a;
            uint32_t *p = c->back + (size_t)y * c->w + x;
            int rr = (int)((*p >> 16 & 255) + (col >> 16 & 255) * g), gg = (int)((*p >> 8 & 255) + (col >> 8 & 255) * g),
                bb = (int)((*p & 255) + (col & 255) * g);
            *p = (uint32_t)(rr > 255 ? 255 : rr) << 16 | (uint32_t)(gg > 255 ? 255 : gg) << 8 | (uint32_t)(bb > 255 ? 255 : bb);
        }
}

/* The wordmark, centred on cx with its baseline on base_y. The dot of the i
   is the core again: it takes the light when the core does. */
static void draw_wordmark(canvas_t *c, const wordmark_t *m, int cx, int base_y, double lit, double a) {
    if (a <= 0) return;
    int x0 = cx - m->w / 2, y0 = base_y - m->base, i = 0;
    uint32_t dot = mix(C_HOT, C_BGLOW, lit);
    glow(c, x0 + m->tx * .5, y0 + m->ty * .5, m->tx * 1.1, C_BRAND, .22 * lit * a);
    for (int k = 0; k < m->n; k++) {
        int b = m->d[k], n = b < 0x40 ? b + 1 : b >= 0xc0 ? b - 0xbf : 1;
        double cov = b < 0x40 ? 0 : b >= 0xc0 ? 1 : (b - 0x40) / 127.0;
        for (int j = 0; cov > 0 && j < n; j++) {
            int mx = (i + j) % m->w, my = (i + j) / m->w, x = x0 + mx, y = y0 + my;
            if (x < 0 || y < 0 || x >= c->w || y >= c->h) continue;
            uint32_t *px = c->back + (size_t)y * c->w + x;
            *px = mix(*px, mx < m->tx && my < m->ty ? dot : C_HOT, cov * a);
        }
        i += n;
    }
}

/* The three system images. Each is a line; the one that is booting is the
   progress line, so there is one thing to read: which image, and how far.
   Not wired to the boot yet: with no slot the row is not drawn. --slot=,
   --progress= and --digest= set them for --render. */
static int g_slot = -1;
static double g_progress = 0;
static char g_digest[40] = "";
static char g_note[96] = "";
static int g_bare;
static void draw_slots(canvas_t *c, const layout_t *l, double a) {
    static const char *name[3] = { "ACTIVE", "PASSIVE", "RECOVERY" };
    int row = 3 * l->seg_w + 2 * l->seg_gap, x0 = (c->w - row) / 2, track = l->trk;
    for (int i = 0; i < 3; i++) {
        int lit = i == g_slot, x = x0 + i * (l->seg_w + l->seg_gap);
        for (int k = 0; k < l->seg_w; k++)
            for (int j = 0; j < l->seg_h; j++) {
                uint32_t *p = c->back + (size_t)(l->seg_y + j) * c->w + x + k;
                *p = mix(*p, lit ? 0x4a4b43 : C_OVERLAY, a);
            }
        if (lit) {
            int w = (int)(l->seg_w * clamp01(g_progress) + .5);
            glow(c, x + w, l->seg_y + l->seg_h * .5, 7 * l->u, C_BRAND, .26 * a);
            for (int k = 0; k < w; k++)
                for (int j = 0; j < l->seg_h; j++) {
                    uint32_t *p = c->back + (size_t)(l->seg_y + j) * c->w + x + k;
                    *p = mix(*p, mix(C_BRAND, C_BGLOW, sstep(w - 26 * l->u, w, k)), a);
                }
        }
        draw_text(c, F_CAPS, name[i], x + (l->seg_w - text_width(F_CAPS, name[i], track)) / 2, l->lab_y, track,
                  lit ? C_SUBTEXT : 0x6c6e64, a);
    }
}

/* Version in the text face, digest in the mono face, as one centred line. */
static void draw_version(canvas_t *c, const layout_t *l, int y, double a) {
    const char *ver = g_ver[0] == 'v' ? g_ver + 1 : g_ver;
    int gap = ver[0] && g_digest[0] ? (int)(16 * l->u) + 2 : 0;
    int wv = ver[0] ? text_width(F_SANS, ver, 0) : 0, wd = g_digest[0] ? text_width(F_MONO, g_digest, 0) : 0;
    int x = (c->w - wv - gap - wd) / 2;
    draw_text(c, F_SANS, ver, x, y, 0, C_LABEL, a);
    draw_text(c, F_MONO, g_digest, x + wv + gap, y, 0, 0x6c6e64, a);
}

/* The passphrase: a title, one round field, one line under it. Each typed
   character is a small light. No device name, no kernel text: the VT stays in
   graphics mode the whole time. */
static void draw_prompt(canvas_t *c, const layout_t *l, double t) {
    int cx = c->w / 2, tu = l->tu, fw = l->field_w;
    const char *title = g_pr.title[0] ? g_pr.title : "Enter the disk passphrase";
    int tf = F_TITLE;
    /* A long title (the recovery key one) drops a size before it is replaced. */
    if (text_width(tf, title, 0) > c->w - 64) tf = F_SANS;
    if (text_width(tf, title, 0) > c->w - 64) title = "Enter the disk passphrase", tf = F_TITLE;
    draw_center(c, tf, title, l->title_y, 0, C_HOT, 1);

    int bad = g_pr.retry && g_pr.st == P_ASK;
    double r = l->field_h * .5, fy = l->field_y + r;
    double bx[2] = { cx - fw * .5 + r, cx + fw * .5 - r }, by[2] = { fy, fy };
    fill_round(c, bx, by, 2, r, bad ? C_ERROR : C_BORDER, bad ? .85 : 1);
    fill_round(c, bx, by, 2, r - tu, 0x151613, 1);
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
            const char *show = vis;
            while (*show && text_width(F_SANS, show, 0) > fw - 4 * (int)r) show++;
            draw_center(c, F_SANS, show, (int)fy + face(F_SANS)->cap / 2, 0, C_HOT, 1);
        } else {
            int n = 0;
            for (size_t i = 0; i < g_pr.len; i++)
                if (((unsigned char)g_pr.buf[i] & 0xC0) != 0x80) n++;
            double step = 18 * l->u, dr = 4.0 * l->u;
            int maxd = (int)((fw - 3 * r) / step);
            if (n > maxd) n = maxd;
            double x = cx - (n - 1) * step * .5;
            for (int i = 0; i < n; i++) {
                double dx = x + i * step;
                glow(c, dx, fy, 3.2 * dr, C_BRAND, .16);
                fill_round(c, &dx, &fy, 1, dr, C_BGLOW, 1);
            }
            if (((int)(t * 2)) & 1) {
                double kx = n ? x + (n - 1) * step + step * .62 : cx;
                fill(c, (int)kx, (int)(fy - 9 * l->u), tu > 1 ? 3 : 2, (int)(18 * l->u), C_BRAND);
            }
        }
    }

    if (g_pr.st == P_CHECKING)
        draw_center(c, F_SANS, "Checking the passphrase...", l->note_y, 0, C_LABEL, 1);
    else if (g_pr.retry)
        draw_center(c, F_SANS, strstr(g_pr.title, "recovery key") ? "Wrong passphrase or recovery key, try again"
                                                                 : "Wrong passphrase, try again", l->note_y, 0, C_ERROR, 1);
    else
        draw_center(c, F_SANS, "Enter to unlock  \xc2\xb7  Esc to clear", l->note_y, 0, 0x777a6f, 1);
}

/* --mark=x,y,r in shares of the width, the height and the height: the build
   uses it to draw the GRUB and lock screen grounds with the same light. */
static double g_mark[3];
static int g_bench;   /* --bench=N with --render: time N frames of the opening, each with the light changed */

static layout_t layout(const canvas_t *c) {
    layout_t l; memset(&l, 0, sizeof l);
    double u = c->h / 1080.0;
    l.u = u;
    g_tier = c->h >= 1500 ? 2 : c->h >= 900;
    l.tu = g_tier == 2 ? 2 : 1;
    l.trk = g_tier + 2;
    l.mr = 118 * u; l.mx = c->w * .5; l.my = floor(c->h * .392) + .5;
    if (g_mark[2] > 0) { l.mx = c->w * g_mark[0]; l.my = c->h * g_mark[1]; l.mr = c->h * g_mark[2]; }
    l.wm = &WORDMARK[c->h < 900 ? 0 : c->h < 1300 ? 1 : c->h < 1900 ? 2 : 3];
    l.base_y = (int)(l.my + l.mr + 60 * u) + l.wm->base;
    l.seg_w = (int)(148 * u); l.seg_gap = (int)(14 * u); l.seg_h = u < 1.4 ? 2 : (int)(2 * u);
    l.seg_y = (int)(c->h * .772);
    l.lab_y = l.seg_y + l.seg_h + (int)(13 * u) + face(F_CAPS)->cap;
    l.ver_y = l.lab_y + (int)(40 * u);
    l.foot_y = c->h - (int)(44 * u);
    l.title_y = (int)(c->h * .728);
    l.field_w = (int)(380 * u); l.field_h = (int)(50 * u);
    if (l.field_w > c->w - 32) l.field_w = c->w - 32;
    l.field_y = l.title_y + (int)(26 * u);
    l.note_y = l.field_y + l.field_h + (int)(32 * u);
    l.pver_y = l.note_y + (int)(46 * u);
    l.top = (int)(l.my - 1.7 * l.mr) - 1;
    if (l.top < 0) l.top = 0;
    l.bottom = l.pver_y + 12 * l.tu;
    return l;
}

/* The story, in seconds. The core lights; the shell takes its light from the
   inside out; the name comes up. Then the sixth side lifts and the light gets
   out, and only then the screen says what is booting. While a passphrase is
   wanted the clock waits just before the lift: the side stays seated and the
   light stays in, until the disk is open. */
#define T_CORE   0.25
#define T_SHELL  0.55
#define T_WORD   1.00
#define T_LIFT   1.55
#define T_INFO   1.95
#define LIFT_SEATED 0.36

static scene_t scene_at(double t) {
    scene_t s; memset(&s, 0, sizeof s);
    s.core = ease_out(seg(t, T_CORE, .45));
    s.power = .70 * ease_out(seg(t, T_CORE, .95)) + .30 * ease_out(seg(t, T_LIFT, .6));
    s.shell = sstep(0, 1, seg(t, T_SHELL, .85));
    s.lift = LIFT_SEATED + (1 - LIFT_SEATED) * ease_back(seg(t, T_LIFT, .60));
    s.flash = t > T_LIFT ? exp(-(t - T_LIFT) * 3.5) * seg(t, T_LIFT, .12) : 0;
    return s;
}

static int g_have_scene;      /* --power= and friends were given */
static scene_t g_scene_set = { -1, -1, -1, -1, 0, {0, 0}, {0, 0}, {0, 0}, {0, 0}, 0, 0 };

/* One frame at t seconds on the story's clock. fade is 1 for full brightness,
   falling to 0 as the splash hands the screen over. Returns 1 when the whole
   ground was repainted (the light moved), 0 when only the rows l->top to
   l->bottom and the foot line changed. */
static int gfx_frame(canvas_t *c, const layout_t *l, double t, double fade, int frame) {
    (void)frame;
    scene_t s = scene_at(t);
    if (g_have_scene) {
        if (g_scene_set.power >= 0) s.power = g_scene_set.power;
        if (g_scene_set.shell >= 0) s.shell = g_scene_set.shell;
        if (g_scene_set.lift >= 0) s.lift = g_scene_set.lift;
        if (g_scene_set.core >= 0) s.core = g_scene_set.core;
        s.flash = 0;
    }
    s.power *= fade;
    scene_derive(&s);
    int full = 0;
    if (!c->lit || fabs(s.power - c->lit_power) > .004 || fabs(s.lift - c->lit_lift) > .004) {
        paint_scene(c, l, &s);
        c->lit = 1; c->lit_power = s.power; c->lit_lift = s.lift;
        memcpy(c->back, c->bg, (size_t)c->w * c->h * 4);
        full = 1;
    } else {
        clear_rect(c, 0, l->top, c->w, l->bottom - l->top);
        clear_rect(c, 0, l->foot_y - 20 * l->tu, c->w, 30 * l->tu);
    }
    /* The core breathes slowly while the boot runs, and answers each key. */
    scene_t m = s;
    if (t > T_INFO) m.power *= 1 + .025 * sin((t - T_INFO) * 2 * M_PI / 4.5);
    m.power = clamp01(m.power + .22 * exp(-(t - g_key_at) * 7));
    draw_mark(c, l, &m, 1);
    if (g_bare) return full;

    double wa = sstep(0, 1, seg(t, T_WORD, .45)) * fade;
    draw_wordmark(c, l->wm, c->w / 2, l->base_y, s.core, wa);

    if (g_pr.st != P_NONE) {
        draw_prompt(c, l, t);
        draw_version(c, l, l->pver_y, fade * .8);
        return full;
    }
    double ia = sstep(0, 1, seg(t, T_INFO, .45)) * fade;
    if (g_slot >= 0) draw_slots(c, l, ia);
    draw_version(c, l, l->ver_y, ia);
    /* The foot line: a note when there is something to say, the key hint
       while D does something, else the family credit. */
    if (g_note[0]) draw_center(c, F_SANS, g_note, l->foot_y, 0, C_WARN, ia);
    else if (g_hotkey) draw_center(c, F_SANS, "Press D for boot details", l->foot_y, 0, C_MUTED, ia);
    else draw_center(c, F_CAPS, "BUILT ON HADRON  \xc2\xb7  BY KAIROS", l->foot_y, l->trk, 0x4b4d45, ia);
    return full;
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
    g_hotkey = hotkey_offer();
    notify_ready();
    int handed = 0;
    const double fade_s = .35;
    double t0 = timeline_start(), started = now_sec(), waited = 0, last = started;
    for (int frame = 0; !g_exit; frame++) {
        double now = now_sec(), t = now - t0, run = now - started, fade = 1;
        /* While the passphrase is wanted the story waits just before the
           lift: the sixth side stays seated until the disk is open. */
        if (g_pr.st != P_NONE && t - waited >= T_LIFT) waited += now - last;
        last = now;
        g_clock = t -= waited;
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
        if (g_want_bootvine) {
            /* bootvine switches to tty12 and draws there; nothing more is
               drawn into the framebuffer after it starts. If it does not
               start, the splash carries on without the hint. */
            g_want_bootvine = 0;
            if (bootvine_start() == 0) { handed = 1; break; }
            g_hotkey = 0;
        }
        if (gfx_frame(&c, &l, t, fade, frame)) blit(&c, 0, c.h);
        else { blit(&c, l.top, l.bottom); blit(&c, l.foot_y - 20 * l.tu, l.foot_y + 10 * l.tu); }
        usleep(FRAME_USEC);
    }
    /* Leave the plain background, not a frozen logo, for whatever comes next. */
    if (c.mem && !handed) { paint_background(&c); memcpy(c.back, c.bg, (size_t)c.w * c.h * 4); blit(&c, 0, c.h); }
    canvas_free(&c);
    agent_unregister();
    tty_text();
    kb_restore();
    return 0;
}

/* --render: draw one frame into a PPM, for the build-time check and for
   previewing the design without booting anything. */
static int render_ppm(const char *path, int w, int h, double t, const char *prompt, int hint) {
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
    if (hint) g_hotkey = 1;
    if (prompt && t > T_LIFT) t = T_LIFT;
    g_clock = t;
    if (prompt && !strcmp(prompt, "key")) g_key_at = t - .04;
    if (g_bench > 0) {
        double a = now_sec();
        for (int i = 0; i < g_bench; i++) { gfx_frame(&c, &l, .3 + 2.0 * i / g_bench, 1, i); blit(&c, 0, c.h); }
        double b = now_sec();
        for (int i = 0; i < g_bench; i++) { gfx_frame(&c, &l, 6, 1, i); blit(&c, l.top, l.bottom); }
        printf("%dx%d: %.1f ms a frame while the light changes, %.1f ms once it stands\n", w, h,
               (b - a) * 1000 / g_bench, (now_sec() - b) * 1000 / g_bench);
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
    const char *render = NULL, *prompt = NULL; int rw = 1280, rh = 800, hint = 0;
    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        if (!strncmp(a, "--duration=", 11)) duration = parse_duration(a + 11);
        else if (!strcmp(a, "--duration") && i + 1 < argc) duration = parse_duration(argv[++i]);
        else if (!strncmp(a, "--render=", 9)) render = a + 9;
        else if (!strncmp(a, "--size=", 7)) sscanf(a + 7, "%dx%d", &rw, &rh);
        else if (!strncmp(a, "--at=", 5)) at = parse_duration(a + 5);
        else if (!strncmp(a, "--prompt=", 9)) prompt = a + 9;
        else if (!strcmp(a, "--hint")) hint = 1;
        /* Values not wired to the boot yet, for --render previews. */
        else if (!strncmp(a, "--slot=", 7)) g_slot = atoi(a + 7);
        else if (!strncmp(a, "--progress=", 11)) g_progress = atof(a + 11);
        else if (!strncmp(a, "--digest=", 9)) snprintf(g_digest, sizeof g_digest, "%s", a + 9);
        else if (!strncmp(a, "--version=", 10)) snprintf(g_ver, sizeof g_ver, "%s", a + 10);
        else if (!strncmp(a, "--note=", 7)) snprintf(g_note, sizeof g_note, "%s", a + 7);
        /* The scene on its own, for the grounds the build draws with the same
           light (GRUB, the lock screen, the installer's desktop). */
        else if (!strcmp(a, "--bare")) g_bare = 1;
        /* The GRUB background tools/gen-grub-bg.py writes: the mark asleep, a
           dark shell lit only on its inner edge. */
        else if (!strcmp(a, "--grub")) {
            g_bare = 1;
            g_mark[0] = .29; g_mark[1] = .54; g_mark[2] = .27;
            g_scene_set.power = .52; g_scene_set.shell = 0; g_scene_set.lift = 1; g_scene_set.core = 1;
            g_have_scene = 1;
        }
        else if (!strncmp(a, "--bench=", 8)) g_bench = atoi(a + 8);
        else if (!strncmp(a, "--ground=", 9)) g_ground = strtol(a + 9, NULL, 16);
        else if (!strncmp(a, "--mark=", 7)) sscanf(a + 7, "%lf,%lf,%lf", &g_mark[0], &g_mark[1], &g_mark[2]);
        else if (!strncmp(a, "--power=", 8)) { g_scene_set.power = atof(a + 8); g_have_scene = 1; }
        else if (!strncmp(a, "--shell=", 8)) { g_scene_set.shell = atof(a + 8); g_have_scene = 1; }
        else if (!strncmp(a, "--lift=", 7)) { g_scene_set.lift = atof(a + 7); g_have_scene = 1; }
        else if (!strncmp(a, "--core=", 7)) { g_scene_set.core = atof(a + 7); g_have_scene = 1; }
        else if (!strcmp(a, "--agent")) g_agent_mode = 1;
        else if (!strcmp(a, "--no-agent")) g_agent_mode = 0;
        /* Anything else is ignored on purpose: a newer kairos-init may pass a
           flag this build does not know, and a splash must never fail a boot
           over that. */
    }
    load_version();
    if (render) return render_ppm(render, rw, rh, at, prompt, hint);
    /* bootvine has the screen (started in the initramfs, it is still
       following the boot): draw nothing over it. */
    if (!access(BOOTVINE_MARK, F_OK)) { notify_ready(); return 0; }
    /* isotope.bootvine: bootvine from the start, no animation. */
    if (cmdline_has("isotope.bootvine") && bootvine_installed() && bootvine_start() == 0) {
        notify_ready();
        return 0;
    }
    if (splash_disabled()) { notify_ready(); return 0; }

    if (!isatty(STDOUT_FILENO)) {
        notify_ready();
        if (g_ver[0]) printf("ISOTOPE %s\n", g_ver); else puts("ISOTOPE");
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
