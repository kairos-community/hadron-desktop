#include <gtk/gtk.h>

#include <errno.h>
#include <fcntl.h>
#include <string.h>
#include <unistd.h>

#include <glib/gstdio.h>

#define STATE_PATH "/run/user/1000/hadron-cua-gtk-state.json"

typedef struct {
    guint clicks;
    guint double_clicks;
    gchar *text;
    gchar *key;
    gboolean dragged;
    gint scroll_value;

    GtkWidget *click_button;
    GtkWidget *double_click_label;
    GtkWidget *named_key_label;
    GtkWidget *drag_target_label;
} Fixture;

static void append_json_string(GString *json, const gchar *value)
{
    const guchar *cursor = (const guchar *)value;

    g_string_append_c(json, '"');
    for (; *cursor != '\0'; cursor++) {
        switch (*cursor) {
        case '"':
            g_string_append(json, "\\\"");
            break;
        case '\\':
            g_string_append(json, "\\\\");
            break;
        case '\b':
            g_string_append(json, "\\b");
            break;
        case '\f':
            g_string_append(json, "\\f");
            break;
        case '\n':
            g_string_append(json, "\\n");
            break;
        case '\r':
            g_string_append(json, "\\r");
            break;
        case '\t':
            g_string_append(json, "\\t");
            break;
        default:
            if (*cursor < 0x20) {
                g_string_append_printf(json, "\\u%04x", *cursor);
            } else {
                g_string_append_c(json, (gchar)*cursor);
            }
            break;
        }
    }
    g_string_append_c(json, '"');
}

static gchar *fixture_state_json(const Fixture *fixture)
{
    GString *json = g_string_new(NULL);

    g_string_append_printf(json,
                           "{\"clicks\":%u,\"double_clicks\":%u,\"text\":",
                           fixture->clicks,
                           fixture->double_clicks);
    append_json_string(json, fixture->text);
    g_string_append(json, ",\"key\":");
    append_json_string(json, fixture->key);
    g_string_append_printf(json,
                           ",\"dragged\":%s,\"scroll_value\":%d}",
                           fixture->dragged ? "true" : "false",
                           fixture->scroll_value);

    return g_string_free(json, FALSE);
}

static gboolean write_all(gint fd, const gchar *contents, gsize length)
{
    while (length > 0) {
        ssize_t written = write(fd, contents, length);

        if (written < 0) {
            if (errno == EINTR) {
                continue;
            }
            return FALSE;
        }
        if (written == 0) {
            errno = EIO;
            return FALSE;
        }
        contents += written;
        length -= (gsize)written;
    }

    return TRUE;
}

static void persist_state(const Fixture *fixture)
{
    gchar *contents = fixture_state_json(fixture);
    gchar *temporary_path = g_strdup_printf("%s.XXXXXX", STATE_PATH);
    gint fd = g_mkstemp_full(temporary_path, O_WRONLY | O_CLOEXEC, 0600);
    gint error_number = 0;

    if (fd < 0) {
        error_number = errno;
    } else {
        if (!write_all(fd, contents, strlen(contents))) {
            error_number = errno;
        } else if (fsync(fd) != 0) {
            error_number = errno;
        }
        if (close(fd) != 0 && error_number == 0) {
            error_number = errno;
        }
        if (error_number == 0 && g_rename(temporary_path, STATE_PATH) != 0) {
            error_number = errno;
        }
    }

    if (error_number != 0) {
        g_warning("unable to atomically write %s: %s",
                  STATE_PATH,
                  g_strerror(error_number));
        g_unlink(temporary_path);
    }

    g_free(temporary_path);
    g_free(contents);
}

static void set_accessible_name(GtkWidget *widget, const gchar *name)
{
    AtkObject *accessible = gtk_widget_get_accessible(widget);

    if (accessible != NULL) {
        atk_object_set_name(accessible, name);
    }
}

static GtkWidget *new_labeled_event_box(const gchar *text, GtkWidget **label)
{
    GtkWidget *event_box = gtk_event_box_new();
    GtkWidget *frame = gtk_frame_new(NULL);

    *label = gtk_label_new(text);
    gtk_label_set_xalign(GTK_LABEL(*label), 0.5f);
    gtk_label_set_yalign(GTK_LABEL(*label), 0.5f);
    gtk_container_add(GTK_CONTAINER(frame), *label);
    gtk_container_add(GTK_CONTAINER(event_box), frame);
    gtk_event_box_set_visible_window(GTK_EVENT_BOX(event_box), TRUE);

    return event_box;
}

static void on_click(GtkButton *button, gpointer user_data)
{
    Fixture *fixture = user_data;
    gchar *label;

    (void)button;
    fixture->clicks++;
    label = g_strdup_printf("Clicks: %u", fixture->clicks);
    gtk_button_set_label(GTK_BUTTON(fixture->click_button), label);
    g_free(label);
    persist_state(fixture);
}

static gboolean on_double_click(GtkWidget *widget,
                                GdkEventButton *event,
                                gpointer user_data)
{
    Fixture *fixture = user_data;

    (void)widget;
    if (event->button == GDK_BUTTON_PRIMARY && event->type == GDK_2BUTTON_PRESS) {
        gchar *label;

        fixture->double_clicks++;
        label = g_strdup_printf("Double clicks: %u", fixture->double_clicks);
        gtk_label_set_text(GTK_LABEL(fixture->double_click_label), label);
        g_free(label);
        persist_state(fixture);
    }

    return FALSE;
}

static void on_text_changed(GtkEditable *editable, gpointer user_data)
{
    Fixture *fixture = user_data;

    g_free(fixture->text);
    fixture->text = g_strdup(gtk_entry_get_text(GTK_ENTRY(editable)));
    persist_state(fixture);
}

static gboolean on_key_press(GtkWidget *widget, GdkEventKey *event, gpointer user_data)
{
    Fixture *fixture = user_data;
    const gchar *key_name = gdk_keyval_name(event->keyval);
    gchar *label;

    (void)widget;
    g_free(fixture->key);
    fixture->key = g_strdup(key_name != NULL ? key_name : "Unknown");
    label = g_strdup_printf("Named key: %s", fixture->key);
    gtk_label_set_text(GTK_LABEL(fixture->named_key_label), label);
    g_free(label);
    persist_state(fixture);

    return FALSE;
}

static void on_drag_data_get(GtkWidget *widget,
                             GdkDragContext *context,
                             GtkSelectionData *selection_data,
                             guint info,
                             guint time,
                             gpointer user_data)
{
    (void)widget;
    (void)context;
    (void)info;
    (void)time;
    (void)user_data;
    gtk_selection_data_set_text(selection_data, "hadron-cua-fixture", -1);
}

static void on_drag_data_received(GtkWidget *widget,
                                  GdkDragContext *context,
                                  gint x,
                                  gint y,
                                  GtkSelectionData *selection_data,
                                  guint info,
                                  guint time,
                                  gpointer user_data)
{
    Fixture *fixture = user_data;
    guchar *payload = gtk_selection_data_get_text(selection_data);
    gboolean accepted = payload != NULL &&
        strcmp((const gchar *)payload, "hadron-cua-fixture") == 0;

    (void)widget;
    (void)x;
    (void)y;
    (void)info;
    g_free(payload);
    fixture->dragged = accepted;
    gtk_label_set_text(GTK_LABEL(fixture->drag_target_label),
                       accepted ? "Drag target: dropped" : "Drag target: waiting");
    persist_state(fixture);
    gtk_drag_finish(context, accepted, FALSE, time);
}

static void on_scroll_changed(GtkAdjustment *adjustment, gpointer user_data)
{
    Fixture *fixture = user_data;
    gdouble value = gtk_adjustment_get_value(adjustment);

    fixture->scroll_value = (gint)(value + 0.5);
    persist_state(fixture);
}

static GtkWidget *new_scrollable_rows(Fixture *fixture)
{
    GtkWidget *scrolled = gtk_scrolled_window_new(NULL, NULL);
    GtkWidget *rows = gtk_box_new(GTK_ORIENTATION_VERTICAL, 0);
    GtkAdjustment *adjustment;

    gtk_scrolled_window_set_policy(GTK_SCROLLED_WINDOW(scrolled),
                                   GTK_POLICY_NEVER,
                                   GTK_POLICY_ALWAYS);
    for (guint row = 1; row <= 40; row++) {
        gchar *text = g_strdup_printf("Scrollable row %02u", row);
        GtkWidget *label = gtk_label_new(text);

        gtk_label_set_xalign(GTK_LABEL(label), 0.0f);
        gtk_widget_set_size_request(label, -1, 36);
        gtk_box_pack_start(GTK_BOX(rows), label, FALSE, FALSE, 0);
        g_free(text);
    }
    gtk_container_add(GTK_CONTAINER(scrolled), rows);
    set_accessible_name(scrolled, "Scrollable rows");
    adjustment = gtk_scrolled_window_get_vadjustment(GTK_SCROLLED_WINDOW(scrolled));
    g_signal_connect(adjustment, "value-changed", G_CALLBACK(on_scroll_changed), fixture);

    return scrolled;
}

int main(int argc, char **argv)
{
    Fixture fixture = {
        .text = g_strdup(""),
        .key = g_strdup(""),
    };
    GtkWidget *window;
    GtkWidget *fixed;
    GtkWidget *double_click_box;
    GtkWidget *entry;
    GtkWidget *named_key_box;
    GtkWidget *drag_source;
    GtkWidget *drag_source_label;
    GtkWidget *drag_target;
    GtkWidget *scrolled;
    GtkTargetEntry drag_targets[] = {
        {"text/plain", 0, 0},
    };

    gtk_init(&argc, &argv);

    window = gtk_window_new(GTK_WINDOW_TOPLEVEL);
    gtk_window_set_title(GTK_WINDOW(window), "Hadron Cua GTK Fixture");
    gtk_window_set_default_size(GTK_WINDOW(window), 900, 640);
    gtk_window_set_resizable(GTK_WINDOW(window), FALSE);
    g_signal_connect(window, "destroy", G_CALLBACK(gtk_main_quit), NULL);
    g_signal_connect(window, "key-press-event", G_CALLBACK(on_key_press), &fixture);

    fixed = gtk_fixed_new();
    gtk_widget_set_size_request(fixed, 900, 640);
    gtk_container_add(GTK_CONTAINER(window), fixed);

    fixture.click_button = gtk_button_new_with_label("Clicks: 0");
    gtk_widget_set_size_request(fixture.click_button, 180, 55);
    set_accessible_name(fixture.click_button, "Click count");
    g_signal_connect(fixture.click_button, "clicked", G_CALLBACK(on_click), &fixture);
    gtk_fixed_put(GTK_FIXED(fixed), fixture.click_button, 40, 35);

    double_click_box = new_labeled_event_box("Double clicks: 0",
                                              &fixture.double_click_label);
    gtk_widget_set_size_request(double_click_box, 180, 70);
    gtk_widget_add_events(double_click_box, GDK_BUTTON_PRESS_MASK);
    set_accessible_name(double_click_box, "Double click count");
    g_signal_connect(double_click_box,
                     "button-press-event",
                     G_CALLBACK(on_double_click),
                     &fixture);
    gtk_fixed_put(GTK_FIXED(fixed), double_click_box, 40, 120);

    entry = gtk_entry_new();
    gtk_entry_set_placeholder_text(GTK_ENTRY(entry), "Type text");
    gtk_widget_set_size_request(entry, 360, 45);
    set_accessible_name(entry, "Text input");
    g_signal_connect(entry, "changed", G_CALLBACK(on_text_changed), &fixture);
    g_signal_connect(entry, "key-press-event", G_CALLBACK(on_key_press), &fixture);
    gtk_fixed_put(GTK_FIXED(fixed), entry, 40, 220);

    named_key_box = new_labeled_event_box("Named key: none", &fixture.named_key_label);
    gtk_widget_set_size_request(named_key_box, 360, 55);
    set_accessible_name(named_key_box, "Named key");
    gtk_fixed_put(GTK_FIXED(fixed), named_key_box, 40, 295);

    drag_source = new_labeled_event_box("Drag source", &drag_source_label);
    gtk_widget_set_size_request(drag_source, 150, 80);
    set_accessible_name(drag_source, "Drag source");
    gtk_drag_source_set(drag_source,
                        GDK_BUTTON1_MASK,
                        drag_targets,
                        G_N_ELEMENTS(drag_targets),
                        GDK_ACTION_COPY);
    g_signal_connect(drag_source,
                     "drag-data-get",
                     G_CALLBACK(on_drag_data_get),
                     &fixture);
    gtk_fixed_put(GTK_FIXED(fixed), drag_source, 40, 420);

    drag_target = new_labeled_event_box("Drag target: waiting", &fixture.drag_target_label);
    gtk_widget_set_size_request(drag_target, 150, 80);
    set_accessible_name(drag_target, "Drag target");
    gtk_drag_dest_set(drag_target,
                      GTK_DEST_DEFAULT_ALL,
                      drag_targets,
                      G_N_ELEMENTS(drag_targets),
                      GDK_ACTION_COPY);
    g_signal_connect(drag_target,
                     "drag-data-received",
                     G_CALLBACK(on_drag_data_received),
                     &fixture);
    gtk_fixed_put(GTK_FIXED(fixed), drag_target, 520, 420);

    scrolled = new_scrollable_rows(&fixture);
    gtk_widget_set_size_request(scrolled, 170, 540);
    gtk_fixed_put(GTK_FIXED(fixed), scrolled, 700, 35);

    gtk_widget_show_all(window);
    persist_state(&fixture);
    gtk_main();

    g_free(fixture.text);
    g_free(fixture.key);
    return 0;
}
