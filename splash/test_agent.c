/* test_agent.c — checks agent.c against the password agent protocol:
 * parsing of ask files, the title the panel shows, the retry rule, and the
 * datagram sent back on a real AF_UNIX socket. `make test` runs it. */
#ifndef _POSIX_C_SOURCE
#define _POSIX_C_SOURCE 200809L
#endif
#ifndef _DEFAULT_SOURCE
#define _DEFAULT_SOURCE
#endif
#include "agent.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <time.h>
#include <unistd.h>

static int fails, checks;
#define CHECK(c) do { checks++; if (!(c)) { fails++; fprintf(stderr, "%s:%d: FAIL %s\n", __FILE__, __LINE__, #c); } } while (0)
#define CHECK_STR(a, b) do { checks++; if (strcmp((a), (b))) { fails++; fprintf(stderr, "%s:%d: FAIL \"%s\" != \"%s\"\n", __FILE__, __LINE__, (a), (b)); } } while (0)

/* What systemd-ask-password --no-tty --id=... writes (src/shared/ask-password-api.c). */
static const char *SYSTEMD_ASK =
    "[Ask]\n"
    "PID=1\n"
    "Socket=/run/systemd/ask-password/sck.0123abcd\n"
    "AcceptCached=0\n"
    "Echo=0\n"
    "NotAfter=0\n"
    "Silent=0\n"
    "Id=hadron-unlock:vda5\n"
    "Message=Enter the passphrase for the Hadron disk (/dev/vda5):\n"
    "Icon=drive-harddisk\n";

static void test_parse(void) {
    ask_t a;
    CHECK(ask_parse(SYSTEMD_ASK, &a) == 0);
    CHECK_STR(a.socket, "/run/systemd/ask-password/sck.0123abcd");
    CHECK_STR(a.id, "hadron-unlock:vda5");
    CHECK_STR(a.message, "Enter the passphrase for the Hadron disk (/dev/vda5):");
    CHECK(a.pid == 1);
    CHECK(a.not_after == 0);
    CHECK(a.echo == 0 && a.silent == 0);

    /* Unknown sections and keys are ignored; keys outside [Ask] do not count;
       CRLF and blanks around '=' are tolerated. */
    const char *odd =
        "# comment\n[Other]\nSocket=/wrong\n\n"
        "[Ask]\r\nFuture = yes\r\nSocket = /run/s\r\nEcho=1\r\nSilent=1\r\n"
        "NotAfter=123456789\r\nMessage=Unlock home\r\n";
    CHECK(ask_parse(odd, &a) == 0);
    CHECK_STR(a.socket, "/run/s");
    CHECK(a.echo == 1 && a.silent == 1);
    CHECK(a.not_after == 123456789ull);
    CHECK_STR(a.message, "Unlock home");
    CHECK_STR(a.id, "");

    CHECK(ask_parse("[Ask]\nMessage=no socket\n", &a) == -1);
    CHECK(ask_parse("Socket=/run/s\n", &a) == -1);   /* no [Ask] section */
    CHECK(ask_parse("", &a) == -1);
}

static void test_expiry(void) {
    ask_t a;
    memset(&a, 0, sizeof a);
    CHECK(!ask_expired(&a, 999999999));       /* NotAfter=0: never */
    a.not_after = 1000;
    CHECK(!ask_expired(&a, 999));
    CHECK(ask_expired(&a, 1001));

    a.pid = getpid();
    CHECK(ask_alive(&a));
    a.pid = 0;
    CHECK(ask_alive(&a));                     /* unknown asker: assume alive */
    a.pid = 0x3ffffff0;                       /* above pid_max: ESRCH */
    CHECK(!ask_alive(&a));
}

static void test_title(void) {
    ask_t a;
    char t[128];
    memset(&a, 0, sizeof a);
#define TITLE(msg, want) do { snprintf(a.message, sizeof a.message, "%s", msg); CHECK_STR(ask_title(&a, t, sizeof t), want); } while (0)
    TITLE("", "Enter the disk passphrase");
    /* PR #60's question: 40 characters without the device, too wide. */
    TITLE("Enter the passphrase for the Hadron disk (/dev/vda5):", "Enter the disk passphrase");
    TITLE("Unlock the Hadron disk (/dev/vda5):", "Unlock the Hadron disk");
    TITLE("Wrong passphrase. Try again (/dev/vda5):", "Enter the disk passphrase");
    TITLE("Please enter passphrase for disk luks-0f1e2d3c (home):", "Enter the disk passphrase");
    TITLE("Passphrase for /dev/sda2:", "Enter the disk passphrase");
    TITLE("Unlock the data disk:", "Unlock the data disk");
    TITLE("Gib das Passwort f\xc3\xbcr die Platte ein", "Enter the disk passphrase");   /* not in the font */
    TITLE("A very long message that will never fit in the panel at any scale", "Enter the disk passphrase");
    /* kcrypt's prompts for a container with a recovery key, first and retry. */
    TITLE("Enter the disk passphrase or recovery key", "Enter the disk passphrase or recovery key");
    TITLE("Wrong passphrase or recovery key, try again", "Enter the disk passphrase or recovery key");
    TITLE("Enter the disk passphrase", "Enter the disk passphrase");
#undef TITLE
}

static void test_retry(void) {
    ask_t a;
    ask_parse(SYSTEMD_ASK, &a);
    CHECK(!ask_is_retry(&a, ""));
    CHECK(ask_is_retry(&a, "hadron-unlock:vda5"));
    CHECK(!ask_is_retry(&a, "kcrypt:vda5"));
    snprintf(a.id, sizeof a.id, "%s", "");
    CHECK(!ask_is_retry(&a, ""));             /* an empty Id never matches */
    snprintf(a.message, sizeof a.message, "%s", "Wrong passphrase. Try again (/dev/vda5):");
    CHECK(ask_is_retry(&a, ""));
}

static void test_format(void) {
    char b[16];
    CHECK(ask_format_reply("hunter2", b, sizeof b) == 8);
    CHECK(!memcmp(b, "+hunter2", 8));
    CHECK(ask_format_reply(NULL, b, sizeof b) == 1);
    CHECK(b[0] == '-');
    CHECK(ask_format_reply("", b, sizeof b) == 1);       /* empty passphrase is still "+" */
    CHECK(b[0] == '+');
    CHECK(ask_format_reply("0123456789abcdef", b, sizeof b) == -1);
}

/* A querier: bind a datagram socket like systemd-ask-password does. */
static int bind_dgram(const char *path) {
    if (strlen(path) >= sizeof ((struct sockaddr_un *)0)->sun_path) return -1;
    struct sockaddr_un sa;
    memset(&sa, 0, sizeof sa);
    sa.sun_family = AF_UNIX;
    memcpy(sa.sun_path, path, strlen(path) + 1);
    unlink(path);
    int fd = socket(AF_UNIX, SOCK_DGRAM, 0);
    if (fd < 0 || bind(fd, (struct sockaddr *)&sa, sizeof sa)) return -1;
    return fd;
}

static double now_s(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec + t.tv_nsec / 1e9;
}

static void write_ask(const char *dir, const char *name, const char *sock, const char *id, const char *extra) {
    char tmp[512], dst[512];
    snprintf(tmp, sizeof tmp, "%s/tmp.%s", dir, name);
    snprintf(dst, sizeof dst, "%s/%s", dir, name);
    FILE *f = fopen(tmp, "w");
    fprintf(f, "[Ask]\nPID=%d\nSocket=%s\nId=%s\nMessage=Enter the passphrase for the Hadron disk (/dev/vda5):\n%s",
            (int)getpid(), sock, id, extra);
    fclose(f);
    rename(tmp, dst);   /* systemd writes a temporary file and renames it */
}

static void test_agent_roundtrip(void) {
    char dir[] = "/tmp/hadron-agent-test.XXXXXX";
    CHECK(mkdtemp(dir) != NULL);
    char sock[256], ask[256];
    snprintf(sock, sizeof sock, "%s/sck.1", dir);
    snprintf(ask, sizeof ask, "%s/ask.1", dir);
    int q = bind_dgram(sock);
    CHECK(q >= 0);

    agent_t ag;
    agent_init(&ag, dir);
    CHECK(agent_poll(&ag, now_s()) == 0);     /* watching, nothing asked */
    CHECK(!ag.active);

    /* A file that is not ask.* is ignored. */
    write_ask(dir, "sck.ignored-not-ask", sock, "x", "");
    CHECK(agent_poll(&ag, now_s()) == 0);

    write_ask(dir, "ask.1", sock, "hadron-unlock:vda5", "");
    CHECK(agent_poll(&ag, now_s()) == 1);
    CHECK(ag.active);
    CHECK_STR(ag.cur.id, "hadron-unlock:vda5");
    CHECK(!ask_is_retry(&ag.cur, ag.last_id));

    CHECK(agent_answer(&ag, "wr0ng") == 0);
    char got[64] = {0};
    ssize_t n = recv(q, got, sizeof got - 1, 0);
    CHECK(n == 6 && !memcmp(got, "+wr0ng", 6));
    CHECK(!ag.active);

    /* The answered file lingers until the asker removes it: no second prompt. */
    CHECK(agent_poll(&ag, now_s() + 2) != 1);
    CHECK(!ag.active);

    /* The asker removes it and asks again with the same Id: a retry. */
    unlink(ask);
    write_ask(dir, "ask.2", sock, "hadron-unlock:vda5", "");
    CHECK(agent_poll(&ag, now_s() + 4) == 1);
    CHECK(ask_is_retry(&ag.cur, ag.last_id));

    /* The file disappearing (the asker timed out) hides the prompt. */
    snprintf(ask, sizeof ask, "%s/ask.2", dir);
    unlink(ask);
    CHECK(agent_poll(&ag, now_s() + 6) == 2);
    CHECK(!ag.active);

    /* An expired NotAfter is never shown. */
    write_ask(dir, "ask.3", sock, "old", "NotAfter=1\n");
    CHECK(agent_poll(&ag, now_s() + 8) == 0);
    CHECK(!ag.active);
    snprintf(ask, sizeof ask, "%s/ask.3", dir);
    unlink(ask);

    /* Cancel sends "-". */
    write_ask(dir, "ask.4", sock, "c", "");
    CHECK(agent_poll(&ag, now_s() + 10) == 1);
    CHECK(agent_answer(&ag, NULL) == 0);
    memset(got, 0, sizeof got);
    n = recv(q, got, sizeof got - 1, 0);
    CHECK(n == 1 && got[0] == '-');
    snprintf(ask, sizeof ask, "%s/ask.4", dir);
    unlink(ask);

    /* A socket nobody listens on is an error, not a crash. */
    CHECK(ask_reply("/nonexistent/sck", "x") < 0);

    agent_close(&ag);
    close(q);
    unlink(sock);
    char p[512];
    snprintf(p, sizeof p, "%s/sck.ignored-not-ask", dir);
    unlink(p);
    CHECK(rmdir(dir) == 0);
}

/* The directory may not exist when the splash starts. */
static void test_agent_late_dir(void) {
    char base[] = "/tmp/hadron-agent-late.XXXXXX";
    CHECK(mkdtemp(base) != NULL);
    char dir[300];
    snprintf(dir, sizeof dir, "%s/ask-password", base);
    agent_t ag;
    agent_init(&ag, dir);
    agent_poll(&ag, now_s());
    struct stat st;
    CHECK(stat(dir, &st) == 0 && S_ISDIR(st.st_mode));
    agent_close(&ag);
    rmdir(dir);
    rmdir(base);
}

int main(void) {
    test_parse();
    test_expiry();
    test_title();
    test_retry();
    test_format();
    test_agent_roundtrip();
    test_agent_late_dir();
    printf("%d checks, %d failed\n", checks, fails);
    return fails ? 1 : 0;
}
