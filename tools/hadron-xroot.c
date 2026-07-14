#include <stdio.h>
#include <stdlib.h>

#include <X11/Xlib.h>

int main(int argc, char **argv) {
    const char *name = argc > 1 ? argv[1] : "#1a1b26";
    Display *display = XOpenDisplay(NULL);
    if (display == NULL) {
        fputs("hadron-xroot: cannot open display\n", stderr);
        return EXIT_FAILURE;
    }

    int screen = DefaultScreen(display);
    Colormap colormap = DefaultColormap(display, screen);
    XColor color;
    XColor exact;
    if (!XAllocNamedColor(display, colormap, name, &color, &exact)) {
        fprintf(stderr, "hadron-xroot: invalid color: %s\n", name);
        XCloseDisplay(display);
        return EXIT_FAILURE;
    }

    Window root = RootWindow(display, screen);
    XSetWindowBackground(display, root, color.pixel);
    XClearWindow(display, root);
    XFlush(display);
    XCloseDisplay(display);
    return EXIT_SUCCESS;
}
