/* hadron-splash — animated hadron-desktop boot splash.
 *
 * Aesthetic: neon-noir cyberpunk rendered in a Tokyo Night blue/cyan ramp,
 * constrained to the 16 colours the kernel VT actually has. The machine wakes:
 * the logo glitches in, a CRT scanline sweeps it, and terse boot lines type out
 * beneath. C99, libc only.
 */
#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif
#ifndef _DEFAULT_SOURCE
#define _DEFAULT_SOURCE
#endif
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/time.h>
#include <unistd.h>

#define DURATION_SEC 5
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

static void load_version(void) {
    FILE *f = fopen("/etc/os-release", "r");
    if (!f) return;
    char b[256];
    while (fgets(b, sizeof(b), f)) {
        if (strncmp(b, "VERSION_ID=", 11)) continue;
        char *v = b + 11; if (*v == '"') v++;
        size_t L = strlen(v);
        while (L && (v[L-1] == '\n' || v[L-1] == '\r' || v[L-1] == '"')) v[--L] = 0;
        snprintf(g_ver, sizeof(g_ver), "v%.40s", v);
        break;
    }
    fclose(f);
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
static const char *TAG = "the immutable tiling desktop";
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

static double now_sec(void) { struct timeval t; gettimeofday(&t, NULL); return t.tv_sec + t.tv_usec/1e6; }

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

int main(void) {
    load_version();

    struct winsize ws; int rows = 24, cols = 80;
    if (ioctl(STDOUT_FILENO, TIOCGWINSZ, &ws) == 0 && ws.ws_row && ws.ws_col) {
        rows = ws.ws_row; cols = ws.ws_col;
    }
    if (!isatty(STDOUT_FILENO) || rows < MIN_ROWS || cols < MIN_COLS) {
        if (g_ver[0]) printf("HADRON %s\n", g_ver); else puts("HADRON");
        return 0;
    }

    grid_t cur, prev;
    if (g_alloc(&cur, rows, cols) || g_alloc(&prev, rows, cols)) return 1;

    struct sigaction sa; memset(&sa, 0, sizeof sa); sigemptyset(&sa.sa_mask);
    sa.sa_handler = on_sig;
    sigaction(SIGINT, &sa, NULL); sigaction(SIGTERM, &sa, NULL);

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
        if (t >= DURATION_SEC) break;

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
