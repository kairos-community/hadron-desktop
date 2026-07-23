package main

// logo is the ANSI-Shadow HADRON wordmark, matching splash/main.c so the boot
// splash and the installer read as the same machine. The block and box-drawing
// glyphs need a console font that covers them — see rootfs/etc/vconsole.conf.
var logo = []string{
	"██╗  ██╗ █████╗ ██████╗ ██████╗  ██████╗ ███╗   ██╗",
	"██║  ██║██╔══██╗██╔══██╗██╔══██╗██╔═══██╗████╗  ██║",
	"███████║███████║██║  ██║██████╔╝██║   ██║██╔██╗ ██║",
	"██╔══██║██╔══██║██║  ██║██╔══██╗██║   ██║██║╚██╗██║",
	"██║  ██║██║  ██║██████╔╝██║  ██║╚██████╔╝██║ ╚████║",
	"╚═╝  ╚═╝╚═╝  ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═══╝",
}
