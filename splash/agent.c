/* agent.c — the systemd password agent protocol. See agent.h. */
#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif
#ifndef _DEFAULT_SOURCE
#define _DEFAULT_SOURCE
#endif
#include "agent.h"

#include <ctype.h>
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/inotify.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define DEFAULT_TITLE "Enter the disk passphrase"
#define REPLY_HELPER  "/usr/lib/systemd/systemd-reply-password"

static void copy_str(char *dst, size_t n, const char *s, size_t len) {
    if (len >= n) len = n - 1;
    memcpy(dst, s, len);
    dst[len] = 0;
}

int ask_parse(const char *text, ask_t *a) {
    char keep_path[sizeof a->path];
    memcpy(keep_path, a->path, sizeof keep_path);
    memset(a, 0, sizeof *a);
    memcpy(a->path, keep_path, sizeof keep_path);

    int in_ask = 0, seen_ask = 0;
    const char *p = text;
    while (*p) {
        const char *eol = strchr(p, '\n');
        size_t len = eol ? (size_t)(eol - p) : strlen(p);
        const char *line = p;
        p = eol ? eol + 1 : p + len;

        while (len && (line[len - 1] == '\r' || line[len - 1] == ' ' || line[len - 1] == '\t')) len--;
        while (len && (*line == ' ' || *line == '\t')) { line++; len--; }
        if (!len || *line == '#' || *line == ';') continue;
        if (*line == '[') {
            in_ask = len == 5 && !strncmp(line, "[Ask]", 5);
            seen_ask |= in_ask;
            continue;
        }
        if (!in_ask) continue;
        const char *eq = memchr(line, '=', len);
        if (!eq) continue;
        size_t kl = (size_t)(eq - line);
        while (kl && (line[kl - 1] == ' ' || line[kl - 1] == '\t')) kl--;
        const char *v = eq + 1;
        size_t vl = len - (size_t)(v - line);
        while (vl && (*v == ' ' || *v == '\t')) { v++; vl--; }

        char val[512];
        copy_str(val, sizeof val, v, vl);
#define KEY(k) (kl == sizeof(k) - 1 && !strncmp(line, k, kl))
        if (KEY("Socket"))        copy_str(a->socket, sizeof a->socket, v, vl);
        else if (KEY("Id"))       copy_str(a->id, sizeof a->id, v, vl);
        else if (KEY("Message"))  copy_str(a->message, sizeof a->message, v, vl);
        else if (KEY("PID"))      a->pid = strtol(val, NULL, 10);
        else if (KEY("NotAfter")) a->not_after = strtoull(val, NULL, 10);
        else if (KEY("Echo"))     a->echo = atoi(val) > 0;
        else if (KEY("Silent"))   a->silent = atoi(val) > 0;
#undef KEY
    }
    return seen_ask && a->socket[0] ? 0 : -1;
}

int ask_parse_file(const char *path, ask_t *a) {
    memset(a, 0, sizeof *a);
    copy_str(a->path, sizeof a->path, path, strlen(path));
    int fd = open(path, O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (fd < 0) return -1;
    struct stat st;
    if (fstat(fd, &st) || !S_ISREG(st.st_mode) || st.st_size > 16384) { close(fd); return -1; }
    char buf[16385];
    ssize_t n = 0, r;
    while (n < (ssize_t)sizeof buf - 1 && (r = read(fd, buf + n, sizeof buf - 1 - n)) > 0) n += r;
    close(fd);
    buf[n] = 0;
    int rc = ask_parse(buf, a);
    a->ino = st.st_ino;
    return rc;
}

int ask_expired(const ask_t *a, uint64_t now_usec) {
    return a->not_after && a->not_after < now_usec;
}

int ask_alive(const ask_t *a) {
    if (a->pid <= 0) return 1;
    return !(kill((pid_t)a->pid, 0) < 0 && errno == ESRCH);
}

#define RECOVERY_TITLE "Enter the disk passphrase or recovery key"

/* kcrypt says "recovery key" when the container has one (a systemd-recovery
   token): the panel then names both, whatever else the message says, and
   also on a retry. The panel draws it smaller when it is too wide. */
int ask_mentions_recovery(const ask_t *a) {
    for (const char *s = a->message; *s; s++)
        if (!strncasecmp(s, "recovery key", 12)) return 1;
    return 0;
}

const char *ask_title(const ask_t *a, char *buf, size_t n) {
    if (ask_mentions_recovery(a)) { copy_str(buf, n, RECOVERY_TITLE, strlen(RECOVERY_TITLE)); return buf; }
    char m[256];
    copy_str(m, sizeof m, a->message, strlen(a->message));
    size_t L = strlen(m);
    /* "... (luks-1234):" or "... (/dev/vda5):" -> "..." */
    while (L && (m[L - 1] == ' ' || m[L - 1] == ':')) m[--L] = 0;
    if (L && m[L - 1] == ')') {
        char *o = strrchr(m, '(');
        if (o) { *o = 0; L = (size_t)(o - m); }
        while (L && (m[L - 1] == ' ' || m[L - 1] == ':')) m[--L] = 0;
    }
    /* 32 columns is what the panel holds next to its padding. */
    int ok = L > 0 && L <= 32 && !strchr(m, '/');
    for (size_t i = 0; ok && i < L; i++)
        if ((unsigned char)m[i] < 32 || (unsigned char)m[i] > 126) ok = 0;
    /* A kcrypt or crypttab name is a device name too. */
    for (size_t i = 0; ok && i + 4 <= L; i++)
        if (!strncasecmp(m + i, "luks", 4)) ok = 0;
    /* A "wrong passphrase" message is shown as the error line instead. */
    for (size_t i = 0; ok && i + 5 <= L; i++)
        if (!strncasecmp(m + i, "wrong", 5)) ok = 0;
    if (!ok) { copy_str(buf, n, DEFAULT_TITLE, strlen(DEFAULT_TITLE)); return buf; }
    copy_str(buf, n, m, L);
    return buf;
}

int ask_is_retry(const ask_t *a, const char *last_answered_id) {
    if (a->id[0] && last_answered_id && !strcmp(a->id, last_answered_id)) return 1;
    for (const char *s = a->message; *s; s++)
        if (!strncasecmp(s, "wrong", 5)) return 1;
    return 0;
}

int ask_format_reply(const char *pass, char *buf, size_t n) {
    size_t pl = pass ? strlen(pass) : 0;
    if (n < pl + 2) return -1;
    buf[0] = pass ? '+' : '-';
    if (pl) memcpy(buf + 1, pass, pl);
    buf[pl + 1] = 0;
    return (int)(pl + 1);
}

static int helper_reply(const char *socket_path, const char *pass) {
    const char *pkexec = access("/usr/bin/pkexec", X_OK) == 0 ? "/usr/bin/pkexec" : NULL;
    if (!pkexec || access(REPLY_HELPER, X_OK)) return -EACCES;
    int pfd[2];
    if (pipe(pfd)) return -errno;
    pid_t pid = fork();
    if (pid < 0) { int e = errno; close(pfd[0]); close(pfd[1]); return -e; }
    if (pid == 0) {
        dup2(pfd[0], STDIN_FILENO);
        close(pfd[0]); close(pfd[1]);
        execl(pkexec, "pkexec", REPLY_HELPER, pass ? "1" : "0", socket_path, (char *)NULL);
        _exit(127);
    }
    close(pfd[0]);
    if (pass) {
        size_t off = 0, len = strlen(pass);
        while (off < len) {
            ssize_t w = write(pfd[1], pass + off, len - off);
            if (w <= 0) break;
            off += (size_t)w;
        }
    }
    close(pfd[1]);
    int st = 0;
    while (waitpid(pid, &st, 0) < 0 && errno == EINTR) {}
    return WIFEXITED(st) && WEXITSTATUS(st) == 0 ? 0 : -EACCES;
}

int ask_reply(const char *socket_path, const char *pass) {
    char msg[4096];
    int len = ask_format_reply(pass, msg, sizeof msg);
    if (len < 0) return -EMSGSIZE;
    struct sockaddr_un sa;
    memset(&sa, 0, sizeof sa);
    sa.sun_family = AF_UNIX;
    if (strlen(socket_path) >= sizeof sa.sun_path) return -ENAMETOOLONG;
    strcpy(sa.sun_path, socket_path);
    int fd = socket(AF_UNIX, SOCK_DGRAM | SOCK_CLOEXEC, 0);
    if (fd < 0) return -errno;
    int rc = 0;
    if (sendto(fd, msg, (size_t)len, MSG_NOSIGNAL, (struct sockaddr *)&sa, sizeof sa) < 0) rc = -errno;
    close(fd);
    memset(msg, 0, sizeof msg);
    if (rc == -EACCES || rc == -EPERM) rc = helper_reply(socket_path, pass);
    return rc;
}

int ask_can_reply(void) {
    if (geteuid() == 0) return 1;
    return access("/usr/bin/pkexec", X_OK) == 0 && access(REPLY_HELPER, X_OK) == 0;
}

/* ---- the watcher ------------------------------------------------------- */

static uint64_t mono_usec(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000u + (uint64_t)t.tv_nsec / 1000u;
}

void agent_init(agent_t *ag, const char *dir) {
    memset(ag, 0, sizeof *ag);
    ag->ifd = -1;
    const char *d = dir ? dir : ASK_DIR;
    copy_str(ag->dir, sizeof ag->dir, d, strlen(d));
}

void agent_close(agent_t *ag) {
    if (ag->ifd >= 0) close(ag->ifd);
    ag->ifd = -1;
    ag->active = 0;
}

static int try_watch(agent_t *ag) {
    if (ag->ifd >= 0) return 0;
    /* systemd's .path units make the directory; the splash can start first. */
    mkdir(ag->dir, 0755);
    int fd = inotify_init1(IN_NONBLOCK | IN_CLOEXEC);
    if (fd < 0) return -1;
    if (inotify_add_watch(fd, ag->dir, IN_CLOSE_WRITE | IN_MOVED_TO | IN_DELETE | IN_MOVED_FROM) < 0) {
        close(fd);
        return -1;
    }
    ag->ifd = fd;
    return 0;
}

static int seen_has(const agent_t *ag, const char *name, ino_t ino) {
    for (int i = 0; i < ag->nseen; i++)
        if (!strcmp(ag->seen[i], name) && ag->seen_ino[i] == ino) return 1;
    return 0;
}

static void seen_add(agent_t *ag, const char *name, ino_t ino) {
    if (ag->nseen == ASK_SEEN_MAX) {
        memmove(ag->seen, ag->seen + 1, sizeof ag->seen[0] * (ASK_SEEN_MAX - 1));
        memmove(ag->seen_ino, ag->seen_ino + 1, sizeof ag->seen_ino[0] * (ASK_SEEN_MAX - 1));
        ag->nseen--;
    }
    copy_str(ag->seen[ag->nseen], sizeof ag->seen[0], name, strlen(name));
    ag->seen_ino[ag->nseen++] = ino;
}

/* Forget answered requests whose file is gone. */
static void seen_prune(agent_t *ag) {
    for (int i = 0; i < ag->nseen;) {
        char p[512];
        struct stat st;
        snprintf(p, sizeof p, "%s/%s", ag->dir, ag->seen[i]);
        if (stat(p, &st) == 0 && st.st_ino == ag->seen_ino[i]) { i++; continue; }
        memmove(ag->seen + i, ag->seen + i + 1, sizeof ag->seen[0] * (size_t)(ag->nseen - i - 1));
        memmove(ag->seen_ino + i, ag->seen_ino + i + 1, sizeof ag->seen_ino[0] * (size_t)(ag->nseen - i - 1));
        ag->nseen--;
    }
}

static const char *base_name(const char *p) {
    const char *s = strrchr(p, '/');
    return s ? s + 1 : p;
}

/* The oldest pending question that nobody answered yet. */
static int pick_next(agent_t *ag) {
    DIR *d = opendir(ag->dir);
    if (!d) return 0;
    struct dirent *e;
    ask_t best, a;
    int have = 0;
    struct timespec best_m = {0, 0};
    uint64_t now = mono_usec();
    while ((e = readdir(d))) {
        if (strncmp(e->d_name, "ask.", 4)) continue;
        char p[512];
        snprintf(p, sizeof p, "%s/%s", ag->dir, e->d_name);
        if (ask_parse_file(p, &a)) continue;
        if (seen_has(ag, e->d_name, a.ino)) continue;
        if (ask_expired(&a, now) || !ask_alive(&a)) continue;
        struct stat st;
        if (stat(p, &st)) continue;
        if (!have || st.st_mtim.tv_sec < best_m.tv_sec ||
            (st.st_mtim.tv_sec == best_m.tv_sec && st.st_mtim.tv_nsec < best_m.tv_nsec)) {
            best = a; best_m = st.st_mtim; have = 1;
        }
    }
    closedir(d);
    if (!have) return 0;
    ag->cur = best;
    ag->active = 1;
    return 1;
}

int agent_poll(agent_t *ag, double now) {
    int changed = 0;
    if (ag->ifd < 0) {
        if (now < ag->next_scan) return 0;
        ag->next_scan = now + 1.0;
        if (try_watch(ag)) return 0;
        changed = 1;
    }
    char buf[4096] __attribute__((aligned(__alignof__(struct inotify_event))));
    ssize_t n;
    while ((n = read(ag->ifd, buf, sizeof buf)) > 0) {
        changed = 1;
        for (char *p = buf; p < buf + n;) {
            struct inotify_event *ev = (struct inotify_event *)p;
            p += sizeof *ev + ev->len;
            if (ev->mask & IN_IGNORED) { close(ag->ifd); ag->ifd = -1; ag->active = 0; return 2; }
        }
    }
    /* Rescan on any event, and once a second anyway, for NotAfter and dead
       askers, which raise no event. */
    if (!changed && now < ag->next_scan) return 0;
    ag->next_scan = now + 1.0;
    seen_prune(ag);

    if (ag->active) {
        struct stat st;
        if (stat(ag->cur.path, &st) || st.st_ino != ag->cur.ino ||
            ask_expired(&ag->cur, mono_usec()) || !ask_alive(&ag->cur)) {
            ag->active = 0;
            return pick_next(ag) ? 1 : 2;
        }
        return 0;
    }
    return pick_next(ag);
}

int agent_answer(agent_t *ag, const char *pass) {
    if (!ag->active) return -ENOENT;
    int rc = ask_reply(ag->cur.socket, pass);
    seen_add(ag, base_name(ag->cur.path), ag->cur.ino);
    if (pass && rc == 0) copy_str(ag->last_id, sizeof ag->last_id, ag->cur.id, strlen(ag->cur.id));
    ag->active = 0;
    ag->next_scan = 0;   /* look for the next question on the next poll */
    return rc;
}
