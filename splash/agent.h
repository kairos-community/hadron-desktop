/* agent.h — the systemd password agent protocol, as kairos-splash speaks it.
 *
 * systemd's docs/PASSWORD_AGENTS.md: a program that needs a password drops an
 * ini file named ask.<random> into /run/systemd/ask-password/ and waits on the
 * AF_UNIX datagram socket the file names. An agent shows the question and
 * sends "+<password>" back, or "-" when the user cancels.
 *
 * This file has no drawing and no terminal code, so test_agent.c can check it
 * on its own. C99, libc and Linux headers only.
 */
#ifndef HADRON_AGENT_H
#define HADRON_AGENT_H

#include <stddef.h>
#include <stdint.h>
#include <sys/types.h>

#define ASK_DIR "/run/systemd/ask-password"

typedef struct {
    char     path[256];     /* the ask.* file */
    char     socket[108];   /* Socket=, sun_path size */
    char     id[128];       /* Id=, "" when absent */
    char     message[256];  /* Message= */
    long     pid;           /* PID=, 0 when absent */
    uint64_t not_after;     /* NotAfter=, CLOCK_MONOTONIC usec, 0 = never */
    int      echo;          /* Echo=1: show what is typed */
    int      silent;        /* Silent=1: show nothing at all for the input */
    ino_t    ino;           /* tells a rewritten file from the one answered */
} ask_t;

/* Parse the text of an ask file. Unknown keys and sections are ignored.
   Returns 0 when it has an [Ask] section with a Socket=, -1 otherwise. */
int ask_parse(const char *text, ask_t *a);

/* Read and parse one file. Same return value; path is copied into a->path. */
int ask_parse_file(const char *path, ask_t *a);

/* 1 when NotAfter= is set and lies before now_usec (CLOCK_MONOTONIC). */
int ask_expired(const ask_t *a, uint64_t now_usec);

/* 0 when the asking process is gone (kill(PID, 0) says ESRCH). */
int ask_alive(const ask_t *a);

/* The panel title: Message= when it is plain ASCII, at most 32 characters,
   and names no
   device (no '/', no "luks"), with a trailing "(...)" and ':' cut off;
   otherwise "Enter the disk passphrase". Writes into buf and returns it. */
int ask_mentions_recovery(const ask_t *a);
const char *ask_title(const ask_t *a, char *buf, size_t n);

/* 1 when this request is a retry after a wrong passphrase: the same non-empty
   Id as the last request answered with a passphrase, or a Message that says
   "wrong". */
int ask_is_retry(const ask_t *a, const char *last_answered_id);

/* Build the datagram: "+" pass, or "-" when pass is NULL. Returns its length,
   or -1 when buf is too small. */
int ask_format_reply(const char *pass, char *buf, size_t n);

/* Send the reply to the asker's socket. Root sends the datagram itself. A
   user that may not write to the socket goes through the protocol's
   privileged helper, pkexec systemd-reply-password, as the document says.
   Returns 0 on success, -errno otherwise. */
int ask_reply(const char *socket_path, const char *pass);

/* 1 when this process can answer at all: root, or pkexec and
   systemd-reply-password are both there. */
int ask_can_reply(void);

/* ---- the watcher ------------------------------------------------------- */

#define ASK_SEEN_MAX 16

typedef struct {
    char  dir[200];
    int   ifd;                    /* inotify fd, -1 until the dir exists */
    int   active;                 /* cur holds a question on screen */
    ask_t cur;
    /* Requests already answered, until their file goes away. The asker
       removes the file after reading the reply, but not at once. */
    char  seen[ASK_SEEN_MAX][64];
    ino_t seen_ino[ASK_SEEN_MAX];
    int   nseen;
    char  last_id[128];           /* Id of the last "+" reply */
    double next_scan;
} agent_t;

/* Watch dir (ASK_DIR when NULL). The directory may not exist yet: the
   watcher creates it, and retries until it can watch it. */
void agent_init(agent_t *ag, const char *dir);
void agent_close(agent_t *ag);

/* Drain inotify, drop the current question if its file went away, its
   NotAfter passed or its asker died, and pick the next one if none is shown.
   now is CLOCK_MONOTONIC in seconds. Returns:
     0  nothing changed
     1  a new question is now in ag->cur (ag->active set)
     2  the current question went away (ag->active cleared) */
int agent_poll(agent_t *ag, double now);

/* Answer the current question (pass NULL to cancel) and forget it. */
int agent_answer(agent_t *ag, const char *pass);

#endif
