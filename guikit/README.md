# guikit

The shared look of Isotope's graphical tools: today the graphical installer
(`install-gui/`), and the tools that must look like it, such as a first-login
setup window. It is a Go module of its own, on Gio. It does not open a
window itself, so `go test ./...` here needs no cgo and no GPU.

## What belongs here

- The night palette (`theme/palette.toml`, as in the installer design note's
  "Visual system" table) and the colour helpers.
- The embedded fonts (Urbanist, Inter, JetBrains Mono, with their
  licences in `fonts/`) and the shared text styles.
- Widgets that know nothing about an app: buttons, text fields, labelled
  fields, radio and checkbox, cards, panels, warnings, links, headings, a
  scrolled page, a pick list, the Isotope mark and the small icons.
- The window frame: the desktop background, the floating window, the sidebar
  with the step list, the page slide, the footer with its buttons, the zoom
  (Ctrl+Plus, Ctrl+Minus, Ctrl+0) and Esc.

## What does not

- Anything an app asks, checks or does: the installer's pages, its use of
  `install-ui/core`, the world map, the QR code, relaunching cage. Those stay
  in the app.
- The words of an app. A widget takes its text as arguments.
- A change of look for one app only. A change here changes every tool:
  check it with the installer's screenshot tests (`install-gui/testdata/`),
  which are the reference for the look.
