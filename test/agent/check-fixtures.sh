#!/usr/bin/env bash
set -euo pipefail

dockerfile=test/agent/Dockerfile.compat
gtk=test/agent/fixtures/gtk3/main.c
chromium=test/agent/fixtures/chromium/index.html

if grep -F -- '--no-gpg-verify' "$dockerfile"; then
    echo 'unsigned remote flag still present' >&2
    exit 1
fi
grep -F '/usr/share/hadron/flathub.flatpakrepo' "$dockerfile" >/dev/null
grep -F 'remotes --system --columns=name,url,options' \
    "$dockerfile" >/dev/null
grep -F 'flatpak-refs.tsv' "$dockerfile" >/dev/null
if grep -F 'columns=ref,active:f,origin,runtime' "$dockerfile"; then
    echo 'Flatpak inventory still uses truncated active commits' >&2
    exit 1
fi
grep -F 'columns=ref,origin,runtime' "$dockerfile" >/dev/null
grep -F 'flatpak info --system --show-commit "$ref"' \
    "$dockerfile" >/dev/null
grep -F "NF != 4" "$dockerfile" >/dev/null

if grep -F 'gtk_selection_data_get_length(selection_data) > 0' "$gtk"; then
    echo 'GTK accepts arbitrary nonempty drag payloads' >&2
    exit 1
fi
grep -F 'gtk_selection_data_get_text(selection_data)' "$gtk" >/dev/null
if grep -F 'g_strerror(errno)' "$gtk"; then
    echo 'GTK persistence reports ambient errno' >&2
    exit 1
fi

if grep -E 'id="drag-source"[^>]*role="button"' "$chromium"; then
    echo 'Chromium drag source still promises button activation' >&2
    exit 1
fi
grep -E 'id="drag-source"[^>]*role="group"' "$chromium" >/dev/null
