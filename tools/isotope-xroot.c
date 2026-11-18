/*
 * isotope-xroot: paint the X root window for the i3 session.
 *
 *     isotope-xroot COLOR [IMAGE.png]
 *
 * With only COLOR, the root window gets that solid colour (the old behaviour).
 * With an IMAGE, the image is scaled to fill each monitor (RandR monitors,
 * centred, cropping the overflow, like swaybg's `fill`) on a COLOR background.
 * If the image cannot be read, isotope-xroot says so and paints COLOR, so a
 * missing wallpaper never leaves the root black.
 *
 * The picture is drawn once into a pixmap that becomes the root background,
 * kept after exit with RetainPermanent, and published in _XROOTPMAP_ID and
 * ESETROOT_PMAP_ID like hsetroot and feh do, so a later run frees the previous
 * pixmap and pseudo-transparent apps can find it.
 */
#include <math.h>
#include <stdio.h>
#include <stdlib.h>

#include <X11/Xatom.h>
#include <X11/Xlib.h>
#include <X11/extensions/Xrandr.h>
#include <cairo-xlib.h>
#include <cairo.h>

/* Free the pixmap a previous run (ours, hsetroot, feh) left on the root. */
static void free_previous(Display *display, Window root, Atom esetroot) {
    Atom type;
    int format;
    unsigned long items, after;
    unsigned char *data = NULL;
    if (XGetWindowProperty(display, root, esetroot, 0, 1, False, AnyPropertyType, &type, &format,
                           &items, &after, &data) == Success &&
        type == XA_PIXMAP && format == 32 && items == 1 && data != NULL) {
        XKillClient(display, *(Pixmap *)data);
    }
    if (data != NULL) {
        XFree(data);
    }
}

static int solid(Display *display, Window root, unsigned long pixel) {
    Atom xrootpmap = XInternAtom(display, "_XROOTPMAP_ID", False);
    Atom esetroot = XInternAtom(display, "ESETROOT_PMAP_ID", False);
    free_previous(display, root, esetroot);
    XDeleteProperty(display, root, xrootpmap);
    XDeleteProperty(display, root, esetroot);
    XSetWindowBackground(display, root, pixel);
    XClearWindow(display, root);
    return EXIT_SUCCESS;
}

static void fill_rect(cairo_t *cr, cairo_surface_t *image, double x, double y, double w, double h) {
    double iw = cairo_image_surface_get_width(image);
    double ih = cairo_image_surface_get_height(image);
    double scale = fmax(w / iw, h / ih);
    cairo_save(cr);
    cairo_rectangle(cr, x, y, w, h);
    cairo_clip(cr);
    cairo_translate(cr, x + (w - iw * scale) / 2, y + (h - ih * scale) / 2);
    cairo_scale(cr, scale, scale);
    cairo_set_source_surface(cr, image, 0, 0);
    cairo_pattern_set_filter(cairo_get_source(cr), CAIRO_FILTER_GOOD);
    cairo_paint(cr);
    cairo_restore(cr);
}

int main(int argc, char **argv) {
    const char *name = argc > 1 ? argv[1] : "#141411";
    const char *path = argc > 2 ? argv[2] : NULL;
    Display *display = XOpenDisplay(NULL);
    if (display == NULL) {
        fputs("isotope-xroot: cannot open display\n", stderr);
        return EXIT_FAILURE;
    }

    int screen = DefaultScreen(display);
    Window root = RootWindow(display, screen);
    Colormap colormap = DefaultColormap(display, screen);
    XColor color;
    XColor exact;
    if (!XAllocNamedColor(display, colormap, name, &color, &exact)) {
        fprintf(stderr, "isotope-xroot: invalid color: %s\n", name);
        XCloseDisplay(display);
        return EXIT_FAILURE;
    }

    cairo_surface_t *image = NULL;
    if (path != NULL) {
        image = cairo_image_surface_create_from_png(path);
        if (cairo_surface_status(image) != CAIRO_STATUS_SUCCESS) {
            fprintf(stderr, "isotope-xroot: cannot load %s: %s; using %s\n", path,
                    cairo_status_to_string(cairo_surface_status(image)), name);
            cairo_surface_destroy(image);
            image = NULL;
        }
    }
    if (image == NULL) {
        int rc = solid(display, root, color.pixel);
        XCloseDisplay(display);
        return rc;
    }

    int width = DisplayWidth(display, screen);
    int height = DisplayHeight(display, screen);
    Pixmap pixmap = XCreatePixmap(display, root, width, height, DefaultDepth(display, screen));
    cairo_surface_t *target =
        cairo_xlib_surface_create(display, pixmap, DefaultVisual(display, screen), width, height);
    cairo_t *cr = cairo_create(target);
    cairo_set_source_rgb(cr, exact.red / 65535.0, exact.green / 65535.0, exact.blue / 65535.0);
    cairo_paint(cr);

    int monitors = 0;
    XRRMonitorInfo *info = NULL;
    int event_base, error_base;
    if (XRRQueryExtension(display, &event_base, &error_base)) {
        info = XRRGetMonitors(display, root, True, &monitors);
    }
    if (info == NULL || monitors < 1) {
        fill_rect(cr, image, 0, 0, width, height);
    } else {
        for (int i = 0; i < monitors; i++) {
            fill_rect(cr, image, info[i].x, info[i].y, info[i].width, info[i].height);
        }
    }
    if (info != NULL) {
        XRRFreeMonitors(info);
    }
    cairo_destroy(cr);
    cairo_surface_flush(target);
    cairo_surface_destroy(target);
    cairo_surface_destroy(image);

    Atom xrootpmap = XInternAtom(display, "_XROOTPMAP_ID", False);
    Atom esetroot = XInternAtom(display, "ESETROOT_PMAP_ID", False);
    XGrabServer(display);
    free_previous(display, root, esetroot);
    XChangeProperty(display, root, xrootpmap, XA_PIXMAP, 32, PropModeReplace,
                    (unsigned char *)&pixmap, 1);
    XChangeProperty(display, root, esetroot, XA_PIXMAP, 32, PropModeReplace,
                    (unsigned char *)&pixmap, 1);
    XSetWindowBackgroundPixmap(display, root, pixmap);
    XClearWindow(display, root);
    XUngrabServer(display);
    /* Keep the pixmap after we exit; the next run frees it via ESETROOT_PMAP_ID. */
    XSetCloseDownMode(display, RetainPermanent);
    XCloseDisplay(display);
    return EXIT_SUCCESS;
}
