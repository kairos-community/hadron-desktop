package main

import (
	"fmt"
	"image"
	"image/color"
	"strconv"
	"strings"
	"time"

	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"

	"github.com/kairos-community/isotope/guikit"
	"github.com/kairos-community/isotope/install-ui/core"
)

// Layout draws one frame.
func (u *UI) Layout(gtx layout.Context) layout.Dimensions {
	gtx = guikit.Scaled(gtx, u.scale)
	u.drain()
	event.Op(gtx.Ops, u)
	u.keys(gtx)
	if u.w != nil {
		u.update(gtx)
	}
	if u.focus != nil {
		gtx.Execute(key.FocusCmd{Tag: u.focus})
		u.focus = nil
	}

	var behind func(layout.Context, image.Rectangle)
	dims := u.win.Layout(gtx, behind, func(gtx layout.Context) layout.Dimensions {
		switch {
		case u.w == nil && u.page < pInstalling:
			return u.blank(gtx)
		case u.page == pDone:
			return u.done(gtx)
		case u.page == pInstalling:
			return u.Frame(gtx, u.steps(), u.slide(u.installing), nil)
		}
		return u.Frame(gtx, u.steps(), u.slide(u.body), u.footer)
	})

	if !u.startedOK {
		u.startedOK = true
		if u.hooks.started != nil {
			u.hooks.started()
		}
	}
	switch {
	case !u.pageAt.IsZero() && u.now().Sub(u.pageAt) < guikit.SlideTime:
		gtx.Execute(op.InvalidateCmd{})
	case u.page == pWelcome || u.page == pInstalling && !u.install.failed:
		// The breathing mark and the spinner: 20 frames a second is
		// plenty for a slow sine, and spares llvmpipe.
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(50 * time.Millisecond)})
	}
	return dims
}

// blank is the window before core has handed over the wizard.
func (u *UI) blank(gtx layout.Context) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return guikit.Mark(gtx, 148, false, 1, 0) })
}

// steps is the sidebar.
func (u *UI) steps() guikit.Steps {
	return guikit.Steps{Title: "Install Isotope", Names: stepNames, Current: stepOf(u.page), Note: u.version}
}

// slide is w sliding in as the page changes.
func (u *UI) slide(w layout.Widget) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions { return guikit.Slide(gtx, u.pageAt, u.now(), w) }
}

func (u *UI) body(gtx layout.Context) layout.Dimensions {
	switch u.page {
	case pWelcome:
		return u.welcome(gtx)
	case pKeyboard:
		return guikit.Pad(gtx, u.keyboard)
	case pAccount:
		return guikit.Pad(gtx, u.account)
	case pDisk:
		return guikit.Pad(gtx, u.disk)
	case pEncrypt:
		return guikit.Pad(gtx, u.encryption)
	case pRecovery:
		return guikit.Pad(gtx, u.recovery)
	case pSummary:
		return guikit.Pad(gtx, u.summary)
	}
	return layout.Dimensions{}
}

// footer is the bar with Go back and Continue.
func (u *UI) footer(gtx layout.Context) layout.Dimensions {
	if u.page == pWelcome {
		return u.Footer(gtx,
			[]guikit.Action{{Click: &u.shutdown, Kind: guikit.ButtonGhost, Label: "Shut down"}},
			func(gtx layout.Context) layout.Dimensions {
				return layout.E.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: 20}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StSmall, guikit.ColDim, "About five minutes")
					})
				})
			},
			[]guikit.Action{{Click: &u.cont, Kind: guikit.ButtonPrimary, Label: "Continue"}})
	}
	kind, txt, enabled := guikit.ButtonPrimary, "Continue", true
	switch u.page {
	case pSummary:
		kind, txt = guikit.ButtonDanger, "Erase and install"
	case pRecovery:
		enabled = u.recoveryConfirmed()
	case pAccount:
		enabled = u.passwordsMatch()
	case pDisk:
		enabled = len(u.w.Disks) > 0
	}
	return u.Footer(gtx, nil, nil, []guikit.Action{
		{Click: &u.back, Kind: guikit.ButtonSecondary, Label: "Go back"},
		{Click: &u.cont, Kind: kind, Label: txt, Disabled: !enabled},
	})
}

// --- 1. Welcome --------------------------------------------------------------

func (u *UI) welcome(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: 40, Bottom: 40, Left: 64, Right: 64}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.W.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, guikit.Dp(gtx, 560))
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					// The mark lit, as the splash leaves it; its side
					// breathes a little out and back.
					return guikit.Mark(gtx, 112, true, 1, 0.5*(1+guikit.Breath(u.now())))
				}),
				guikit.VGap(36),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.Label(gtx, guikit.StHero, guikit.ColText, "Welcome to Isotope")
				}),
				guikit.VGap(14),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, guikit.Dp(gtx, 500))
					return u.LabelLines(gtx, guikit.StLead, guikit.ColText2, 1.55, "An immutable Linux desktop, built on Hadron. The system is a read-only image. An update arrives as a whole new image, and a bad one rolls back by itself.")
				}),
				guikit.VGap(40),
				layout.Rigid(u.images),
			)
		})
	})
}

// images is the welcome page's row of the three system images, the row the
// splash draws while booting: the one you run, the one that takes the next
// update, and the way back.
func (u *UI) images(gtx layout.Context) layout.Dimensions {
	names := [3][2]string{{"ACTIVE", "runs your desktop"}, {"PASSIVE", "takes the next update"}, {"RECOVERY", "the way back"}}
	var kids []layout.FlexChild
	for i, n := range names {
		i, n := i, n
		if i > 0 {
			kids = append(kids, layout.Rigid(layout.Spacer{Width: 14}.Layout))
		}
		kids = append(kids, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			line, name, sub := guikit.ColRing, guikit.ColDim, guikit.ColDim
			if i == 0 {
				line, name, sub = guikit.ColAccent, guikit.ColText, guikit.ColText2
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					h := guikit.Dp(gtx, 2)
					guikit.FillRect(gtx, image.Rect(0, 0, gtx.Constraints.Max.X, h), line)
					return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, h)}
				}),
				guikit.VGap(10),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.Label(gtx, guikit.StCaps, name, spaced(n[0]))
				}),
				guikit.VGap(4),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.Label(gtx, guikit.StSmall, sub, n[1])
				}),
			)
		}))
	}
	return layout.Flex{}.Layout(gtx, kids...)
}

// spaced letter-spaces caps with thin spaces, as the splash does: Gio has no
// tracking.
func spaced(s string) string {
	r := []rune(s)
	out := make([]rune, 0, 2*len(r))
	for i, c := range r {
		if i > 0 {
			out = append(out, '\u2009')
		}
		out = append(out, c)
	}
	return string(out)
}

// --- 2. Keyboard and time zone ----------------------------------------------

func (u *UI) keyboard(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.Heading("Keyboard and time zone", "The layout applies at once, here and on the disk passphrase prompt at every start.")),
		guikit.VGap(22),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, u.keyboardColumn),
				layout.Rigid(layout.Spacer{Width: 20}.Layout),
				layout.Flexed(1, u.zoneColumn),
			)
		}),
	)
}

func (u *UI) keyboardColumn(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Label(gtx, guikit.StLabel, guikit.ColText2, "Keyboard layout")
		}),
		guikit.VGap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Input(gtx, u.kbSearch, guikit.StBody, 38, "", nil)
		}),
		guikit.VGap(10),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.PickList(gtx, &u.kbList, u.kbShown, u.kbSel, u.kbRows, func(i int) (string, string) {
				k := u.w.Keyboards[i]
				return k.Description, k.Code()
			})
		}),
		guikit.VGap(14),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Label(gtx, guikit.StSmall, guikit.ColDim, "Try it")
		}),
		guikit.VGap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.Input(gtx, u.kbTry, guikit.StBody, 38, "", nil) }),
	)
}

func (u *UI) zoneColumn(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Label(gtx, guikit.StLabel, guikit.ColText2, "Time zone")
		}),
		guikit.VGap(10),
		// The map, or while a city is being typed the zones it matches.
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if u.zoneFiltering() || len(u.zoneCoords) == 0 {
				return u.PickList(gtx, &u.tzList, u.tzShown, u.tzSel, u.tzRows, func(i int) (string, string) {
					return strings.ReplaceAll(u.w.Zones[i], "_", " "), ""
				})
			}
			gtx.Constraints.Min = gtx.Constraints.Max
			return guikit.Panel(gtx, 10, layout.Inset{}, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = gtx.Constraints.Max
				defer clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Max}, guikit.Dp(gtx, 10)).Push(gtx.Ops).Pop()
				return u.worldMap(gtx)
			})
		}),
		guikit.VGap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Input(gtx, u.tzField, guikit.StBody, 38, "", func(gtx layout.Context) layout.Dimensions {
				return u.Label(gtx, guikit.StSmall, guikit.ColDim, u.zoneClock())
			})
		}),
		guikit.VGap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if u.tzField.Err != "" {
				return u.Label(gtx, guikit.StSmall, guikit.ColError, u.tzField.Err)
			}
			hint := "Found from this computer's clock. Click the map or type a city."
			if len(u.zoneCoords) == 0 {
				hint = "Found from this computer's clock. Type a city to change it."
			}
			return u.LabelLines(gtx, guikit.StSmall, guikit.ColDim, 1.4, hint)
		}),
	)
}

// zoneClock is "UTC+01:00 · 13:04" for the chosen zone, or nothing when the
// zone cannot be loaded.
func (u *UI) zoneClock() string {
	if u.tzSel < 0 {
		return ""
	}
	loc, err := time.LoadLocation(u.w.Zones[u.tzSel])
	if err != nil {
		return ""
	}
	t := u.now().In(loc)
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("UTC%s%02d:%02d · %s", sign, off/3600, off%3600/60, t.Format("15:04"))
}

// --- 3. Computer and account ------------------------------------------------

func (u *UI) account(gtx layout.Context) layout.Dimensions {
	return layout.Flex{}.Layout(gtx,
		layout.Rigid(u.avatar),
		layout.Rigid(layout.Spacer{Width: 36}.Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return guikit.Scrolled(gtx, &u.acctList, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(u.Heading("Computer and account", "This account is the computer's administrator: it installs updates and extensions.")),
					guikit.VGap(16),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Labelled(gtx, "Computer name", u.host, "", func(gtx layout.Context) layout.Dimensions {
							return u.Input(gtx, u.host, guikit.StBody, 38, "", nil)
						})
					}),
					guikit.VGap(16),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Labelled(gtx, "User name", u.user, "Lowercase letters, digits, - or _, up to 32 characters.", func(gtx layout.Context) layout.Dimensions {
							return u.Input(gtx, u.user, guikit.StBody, 38, "", nil)
						})
					}),
					guikit.VGap(16),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						hint := ""
						if u.pw2.Editor.Len() > 0 && !u.passwordsMatch() {
							hint = "The passwords do not match yet."
						}
						return guikit.TwoCols(gtx,
							func(gtx layout.Context) layout.Dimensions {
								return u.Labelled(gtx, "Password", u.pw, "", func(gtx layout.Context) layout.Dimensions {
									return u.Input(gtx, u.pw, guikit.StBody, 38, "", nil)
								})
							},
							func(gtx layout.Context) layout.Dimensions {
								return u.Labelled(gtx, "Type it again", u.pw2, hint, func(gtx layout.Context) layout.Dimensions {
									return u.Input(gtx, u.pw2, guikit.StBody, 38, "", nil)
								})
							})
					}),
					guikit.VGap(20),
					layout.Rigid(u.githubBox),
				)
			})
		}),
	)
}

// avatar is the account page's left column: the user's initial, name and
// computer name, as typed.
func (u *UI) avatar(gtx layout.Context) layout.Dimensions {
	gtx.Constraints = layout.Exact(image.Pt(guikit.Dp(gtx, 150), gtx.Constraints.Max.Y))
	name := strings.TrimSpace(u.user.Editor.Text())
	host := strings.TrimSpace(u.host.Editor.Text())
	if host == "" {
		host = core.DefaultHostname
	}
	initial := "?"
	if name != "" {
		initial = strings.ToUpper(name[:1])
	}
	return layout.Inset{Top: 60}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				s := guikit.Dp(gtx, 104)
				b := guikit.Box{Fill: guikit.ColField, Border: guikit.Over(guikit.ColWindow, guikit.ColText, 0.08), BorderWidth: 1, Radius: 52}
				b.Paint(gtx, image.Pt(s, s))
				gtx.Constraints = layout.Exact(image.Pt(s, s))
				return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.Label(gtx, stAvatar, guikit.ColGlow, initial)
				})
			}),
			guikit.VGap(10),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				if name == "" {
					name = " "
				}
				return u.LabelMax(gtx, guikit.StSmall, guikit.ColText2, 1, name)
			}),
			guikit.VGap(10),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return u.LabelMax(gtx, guikit.StMono12, guikit.ColDim, 1, "@"+host)
			}),
		)
	})
}

func (u *UI) githubBox(gtx layout.Context) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return guikit.Panel(gtx, 10, layout.Inset{Top: 14, Bottom: 14, Left: 16, Right: 16}, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return keyIcon(gtx, 16) }),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StLabel, guikit.ColText2, "SSH keys from GitHub ")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StSmall, guikit.ColDim, "(optional)")
					}),
				)
			}),
			guikit.VGap(10),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.Input(gtx, u.gh, guikit.StMono, 38, "github.com/", nil)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if u.gh.Err != "" {
					return layout.Inset{Top: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StTiny, guikit.ColError, u.gh.Err)
					})
				}
				txt, good, ok := u.githubStatus()
				if !ok {
					return layout.Dimensions{}
				}
				// Warnings, not errors: the install goes on without keys.
				return layout.Inset{Top: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if good {
								return guikit.CheckIcon(gtx, 12)
							}
							if txt == "Checking GitHub" {
								return guikit.Spinner(gtx, 12, float32(u.now().UnixMilli()%1000)/1000)
							}
							return guikit.WarnIcon(gtx, 13, guikit.ColWarn)
						}),
						layout.Rigid(layout.Spacer{Width: 6}.Layout),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							col := guikit.ColText2
							if good {
								col = guikit.ColGlow
							}
							return u.Label(gtx, guikit.StSmall, col, txt)
						}),
					)
				})
			}),
		)
	})
}

// --- 4. Disk -----------------------------------------------------------------

func (u *UI) disk(gtx layout.Context) layout.Dimensions {
	if len(u.w.Disks) == 0 {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.Heading("Where should Isotope go?", "No disk was found. Connect one, then rescan.")),
			guikit.VGap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.Button(gtx, &u.rescan, guikit.ButtonSecondary, "Rescan", true)
			}),
		)
	}
	info := u.info(u.diskSel)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.Heading("Where should Isotope go?", "Isotope uses the whole disk. Everything on the disk you pick is erased.")),
		guikit.VGap(22),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			rows := (len(u.w.Disks) + 2) / 3
			return u.diskList.Layout(gtx, rows+1, func(gtx layout.Context, r int) layout.Dimensions {
				if r == rows {
					return u.diskBar(gtx, u.diskSel)
				}
				return layout.Inset{Bottom: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					var kids []layout.FlexChild
					for c := 0; c < 3; c++ {
						i := r*3 + c
						if c > 0 {
							kids = append(kids, layout.Rigid(layout.Spacer{Width: 14}.Layout))
						}
						kids = append(kids, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							if i >= len(u.w.Disks) {
								return layout.Dimensions{}
							}
							return u.diskCard(gtx, i)
						}))
					}
					return layout.Flex{}.Layout(gtx, kids...)
				})
			})
		}),
		guikit.VGap(14),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Warning(gtx, info.ErasureWarning())
		}),
	)
}

// info is what is known about Disks[i], with its size from the disk list
// when lsblk gave none.
func (u *UI) info(i int) core.DiskInfo {
	var d core.DiskInfo
	if i < len(u.diskInfo) {
		d = u.diskInfo[i]
	}
	if d.SizeBytes == 0 {
		// core's SizeGB counts 2^30 bytes (sectors / 2 / 1024 / 1024).
		d.SizeBytes = int64(u.w.Disks[i].SizeGB) << 30
	}
	return d
}

// diskTitle is the card's title: the model, or the size when sysfs has no
// model.
func (u *UI) diskTitle(i int) string {
	if m := u.info(i).Model; m != "" {
		return m
	}
	return core.HumanSize(u.info(i).SizeBytes) + " disk"
}

// diskLine is "1 TB · NVMe".
func (u *UI) diskLine(i int) string {
	s := core.HumanSize(u.info(i).SizeBytes)
	if b := u.info(i).Bus; b != "" {
		s += " · " + b
	}
	return s
}

func (u *UI) diskCard(gtx layout.Context, i int) layout.Dimensions {
	d := u.w.Disks[i]
	on := i == u.diskSel
	usb := u.info(i).Bus == "USB"
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return u.Card(gtx, &u.diskCards[i], on, layout.UniformInset(18), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return diskIcon(gtx, 48, on, usb) }),
			guikit.VGap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.LabelMax(gtx, guikit.StCard, guikit.ColText, 1, u.diskTitle(i))
			}),
			guikit.VGap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.Label(gtx, guikit.StSmall, guikit.ColText2, u.diskLine(i))
			}),
			guikit.VGap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.Label(gtx, guikit.StMono12, guikit.ColDim, d.Path)
			}),
		)
	})
}

// segColors are the disk bar's partitions, in turn; free space is the
// field colour.
var segColors = []func() color.NRGBA{
	func() color.NRGBA { return guikit.ColRing },
	func() color.NRGBA { return guikit.RGB(0x4a4857) },
	func() color.NRGBA { return guikit.RGB(0x6e6a80) },
	func() color.NRGBA { return guikit.RGB(0x3f3d4b) },
}

// diskBar is what is on the chosen disk now: one segment per partition, and
// their names and sizes under it.
func (u *UI) diskBar(gtx layout.Context, i int) layout.Dimensions {
	info := u.info(i)
	segs := info.Segments()
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return guikit.Panel(gtx, 12, layout.Inset{Top: 16, Bottom: 16, Left: 18, Right: 18}, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StLabel, guikit.ColText2, "On "+u.w.Disks[i].Path+" now")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StSmall, guikit.ColDim, core.HumanSize(info.SizeBytes))
					}),
				)
			}),
			guikit.VGap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				w, h := gtx.Constraints.Max.X, guikit.Dp(gtx, 14)
				defer clip.UniformRRect(image.Rect(0, 0, w, h), h/2).Push(gtx.Ops).Pop()
				if len(segs) == 0 {
					guikit.FillRect(gtx, image.Rect(0, 0, w, h), guikit.ColField)
					return layout.Dimensions{Size: image.Pt(w, h)}
				}
				gap := guikit.Dp(gtx, 2)
				avail := w - gap*(len(segs)-1)
				x := 0
				for n, sg := range segs {
					sw := int(float64(avail) * float64(sg.SizeBytes) / float64(info.SizeBytes))
					if sw < guikit.Dp(gtx, 3) {
						sw = guikit.Dp(gtx, 3)
					}
					if n == len(segs)-1 || x+sw > w {
						sw = w - x
					}
					c := guikit.ColField
					if !sg.Free {
						c = segColors[n%len(segColors)]()
					}
					guikit.FillRect(gtx, image.Rect(x, 0, x+sw, h), c)
					x += sw + gap
				}
				return layout.Dimensions{Size: image.Pt(w, h)}
			}),
			guikit.VGap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				var parts []string
				for _, sg := range segs {
					parts = append(parts, sg.Kind+" · "+core.HumanSize(sg.SizeBytes))
				}
				switch {
				case !info.Read:
					parts = []string{"What is on this disk could not be read."}
				case len(parts) == 0:
					parts = []string{"No partitions"}
				}
				return u.LabelLines(gtx, guikit.StTiny, guikit.ColDim, 1.5, strings.Join(parts, "     "))
			}),
		)
	})
}

// --- 5. Encryption -----------------------------------------------------------

// The Trusted Boot recovery key text is the text installer's, shortened to
// fit a card.
const tpmText = "This computer unlocks its disk with its TPM security chip, without asking you anything. If the TPM is reset or a firmware update changes what it checks, your files cannot be opened without a recovery key. The computer asks for the key only then."

func (u *UI) encryption(gtx layout.Context) layout.Dimensions {
	if u.w.TrustedBoot {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.Heading("Create a recovery key?", tpmText)),
			guikit.VGap(18),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return guikit.TwoCols(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return u.ChoiceCard(gtx, &u.encNo, !u.recKey, "No, the TPM only", "Nothing to keep safe.")
					},
					func(gtx layout.Context) layout.Dimensions {
						return u.ChoiceCard(gtx, &u.encYes, u.recKey, "Create a recovery key", "You see it once, on the next screen.")
					})
			}),
			layout.Rigid(u.encError),
		)
	}
	kids := []layout.FlexChild{
		layout.Rigid(u.Heading("Encrypt your files?", "With encryption, nobody can read your files from a lost or stolen disk. You type a passphrase every time the computer starts.")),
		guikit.VGap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return guikit.TwoCols(gtx,
				func(gtx layout.Context) layout.Dimensions {
					return u.ChoiceCard(gtx, &u.encNo, !u.encOn, "Don't encrypt", "Starts without a passphrase.")
				},
				func(gtx layout.Context) layout.Dimensions {
					return u.ChoiceCard(gtx, &u.encYes, u.encOn, "Encrypt my files", "A passphrase at every start.")
				})
		}),
	}
	if u.encOn {
		kids = append(kids,
			guikit.VGap(18),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return guikit.TwoCols(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return u.Labelled(gtx, "Disk passphrase", u.pp, "", func(gtx layout.Context) layout.Dimensions {
							return u.Input(gtx, u.pp, guikit.StBody, 38, "", nil)
						})
					},
					func(gtx layout.Context) layout.Dimensions {
						return u.Labelled(gtx, "Type it again", u.pp2, "", func(gtx layout.Context) layout.Dimensions {
							return u.Input(gtx, u.pp2, guikit.StBody, 38, "", nil)
						})
					})
			}),
			guikit.VGap(18),
			layout.Rigid(u.strength),
			guikit.VGap(18),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.Label(gtx, guikit.StTiny, guikit.ColDim, fmt.Sprintf("%d characters or more. A few words work well.", core.MinPassphraseLen))
			}),
			guikit.VGap(18),
			layout.Rigid(u.recoveryBox),
		)
	}
	kids = append(kids, layout.Rigid(u.encError))
	return guikit.Scrolled(gtx, &u.encList, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	})
}

func (u *UI) encError(gtx layout.Context) layout.Dimensions {
	if u.encErr == "" {
		return layout.Dimensions{}
	}
	return layout.Inset{Top: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return u.Label(gtx, guikit.StSmall, guikit.ColError, u.encErr)
	})
}

// strength is the meter under the passphrase: core.PassphraseStrength in
// four bars and its words.
func (u *UI) strength(gtx layout.Context) layout.Dimensions {
	s := core.PassphraseStrength(u.pp.Editor.Text())
	bars, col, txtCol := 0, guikit.ColAccent, guikit.ColGlow
	switch {
	case s == "":
	case strings.HasPrefix(s, "too short"):
		bars, col, txtCol = 1, guikit.ColError, guikit.ColErrorText
	case strings.HasPrefix(s, "weak"):
		bars, col, txtCol = 2, guikit.ColError, guikit.ColErrorText
	case strings.HasPrefix(s, "fair"):
		bars = 3
	default:
		bars = 4
	}
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			w := gtx.Constraints.Max.X
			gap := guikit.Dp(gtx, 4)
			bw := (w - 3*gap) / 4
			h := guikit.Dp(gtx, 5)
			for i := 0; i < 4; i++ {
				c := guikit.ColField
				if i < bars {
					c = col
				}
				x := i * (bw + gap)
				guikit.FillRRect(gtx, image.Rect(x, 0, x+bw, h), guikit.Dp(gtx, 3), c)
			}
			return layout.Dimensions{Size: image.Pt(w, h)}
		}),
		layout.Rigid(layout.Spacer{Width: 12}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(guikit.Dp(gtx, 240), gtx.Sp(16)))
			return u.Label(gtx, guikit.StSmall, txtCol, s)
		}),
	)
}

func (u *UI) recoveryBox(gtx layout.Context) layout.Dimensions {
	return u.CheckCard(gtx, &u.recBox, u.recKey, "Also create a recovery key", "A second way in if you forget the passphrase. You see it once, on the next screen.")
}

// --- 6. Recovery key ---------------------------------------------------------

func (u *UI) recovery(gtx layout.Context) layout.Dimensions {
	groups := core.RecoveryKeyGroups(u.w.Choice.RecoveryKeyValue)
	ask := u.w.ConfirmGroup
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.Heading("Your recovery key", "Write it down or take a photo of it now. It is shown only this once and saved nowhere.")),
		guikit.VGap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			b := guikit.Box{Radius: 14, Fill: guikit.ColKeyBox, Border: guikit.Over(guikit.ColKeyBox, guikit.ColGlow, 0.25), BorderWidth: 1}
			return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 22, Bottom: 22, Left: 24, Right: 24}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					row := func(r int) layout.FlexChild {
						return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							var kids []layout.FlexChild
							for c := 0; c < 4; c++ {
								i := r*4 + c
								if c > 0 {
									kids = append(kids, layout.Rigid(layout.Spacer{Width: 20}.Layout))
								}
								kids = append(kids, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									g := ""
									if i < len(groups) {
										g = groups[i]
									}
									return u.keyGroup(gtx, i, g, i == ask)
								}))
							}
							return layout.Flex{}.Layout(gtx, kids...)
						})
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, row(0), guikit.VGap(16), row(1))
				})
			})
		}),
		guikit.VGap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Label(gtx, guikit.StTiny, guikit.ColDim, "Only these letters appear: "+strings.Join(strings.Split(core.Modhex, ""), " "))
		}),
		guikit.VGap(22),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.End}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Max.X = guikit.Dp(gtx, 280)
					return u.Labelled(gtx, fmt.Sprintf("To check you saved it, type group %d", ask+1), u.rk, "", func(gtx layout.Context) layout.Dimensions {
						return u.Input(gtx, u.rk, stMonoField, 40, "", nil)
					})
				}),
				layout.Rigid(layout.Spacer{Width: 14}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					txt := "Show as QR code"
					if u.showQR {
						txt = "Hide the QR code"
					}
					return u.qrToggle(gtx, txt)
				}),
				layout.Rigid(layout.Spacer{Width: 18}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if !u.showQR {
						return layout.Dimensions{}
					}
					// Drawn in the window only, like the key: never saved.
					return qrCode(gtx, u.w.Choice.RecoveryKeyValue, 148)
				}),
			)
		}),
	)
}

// qrToggle is the small outlined button with the QR glyph.
func (u *UI) qrToggle(gtx layout.Context, txt string) layout.Dimensions {
	b := guikit.Box{Radius: 8, Border: guikit.Over(guikit.ColWindow, guikit.ColText, 0.1), BorderWidth: 1}
	if gtx.Focused(&u.qrButton) {
		b.Border, b.Ring, b.RingWidth = guikit.ColAccent, guikit.FocusRing, 3
	}
	return u.qrButton.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.Y = guikit.Dp(gtx, 40)
		return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 14, Right: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return layout.W.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.Y = 0
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return qrGlyph(gtx, 14) }),
						layout.Rigid(layout.Spacer{Width: 8}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.Label(gtx, guikit.StSmall500, guikit.ColText2, txt)
						}),
					)
				})
			})
		})
	})
}

func (u *UI) keyGroup(gtx layout.Context, i int, g string, asked bool) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Label(gtx, stMonoIndex, guikit.ColDim, fmt.Sprint(i+1))
		}),
		guikit.VGap(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			col := guikit.ColGlow
			if asked {
				col = guikit.ColText
			}
			dims := u.Label(gtx, stMonoKey, col, g)
			if asked {
				y := dims.Size.Y + guikit.Dp(gtx, 4)
				guikit.FillRect(gtx, image.Rect(0, y, dims.Size.X, y+guikit.Dp(gtx, 1)), guikit.ColAccent)
			}
			return dims
		}),
	)
}

// --- 7. Summary ---------------------------------------------------------------

func (u *UI) summary(gtx layout.Context) layout.Dimensions {
	c := u.w.Choice
	type row struct {
		name, value, code string
		page              gpage
		danger            bool
	}
	kb := c.KeyboardLayout
	if i := u.w.KeyboardIndex(); i >= 0 {
		kb = u.w.Keyboards[i].Description
	}
	code := c.KeyboardLayout
	if c.KeyboardVariant != "" {
		code += "(" + c.KeyboardVariant + ")"
	}
	acct := c.Username
	if c.Github != "" {
		keys := "SSH keys"
		if g := u.ghState; g.answered == c.Github && g.err == nil && g.n > 0 {
			keys = strconv.Itoa(g.n) + " SSH keys"
			if g.n == 1 {
				keys = "1 SSH key"
			}
		}
		acct += " · " + keys + " from github.com/" + c.Github
	}
	enc := "Off"
	encPage := pEncrypt
	switch {
	case c.Encrypt && c.RecoveryKey:
		enc = "On, passphrase at every start · recovery key saved"
	case c.Encrypt:
		enc = "On, passphrase at every start"
	case u.w.TrustedBoot && c.RecoveryKey:
		enc = "On, unlocked by the TPM · recovery key saved"
	case u.w.TrustedBoot:
		enc = "On, unlocked by the TPM"
	}
	diskVal := c.Disk
	for i, d := range u.w.Disks {
		if d.Path == c.Disk {
			diskVal = core.HumanSize(u.info(i).SizeBytes) + " · "
			if m := u.info(i).Model; m != "" {
				diskVal = m + " · " + diskVal
			}
		}
	}
	rows := []row{
		{"Keyboard", kb, code, pKeyboard, false},
		{"Time zone", c.Timezone, "", pKeyboard, false},
		{"Computer name", c.Hostname, "", pAccount, false},
		{"Account", acct, "", pAccount, false},
		{"Encryption", enc, "", encPage, false},
		{"Disk", diskVal, c.Disk, pDisk, true},
	}
	u.summaryChanges = u.summaryChanges[:0]
	for _, r := range rows {
		u.summaryChanges = append(u.summaryChanges, r.page)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.Heading("Ready to install", "Check what you picked. Click Change to go back to it.")),
		guikit.VGap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return guikit.Panel(gtx, 12, layout.Inset{}, func(gtx layout.Context) layout.Dimensions {
				var kids []layout.FlexChild
				for i, r := range rows {
					i, r := i, r
					kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						dims := layout.Inset{Top: 12, Bottom: 12, Left: 18, Right: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									gtx.Constraints = layout.Exact(image.Pt(guikit.Dp(gtx, 152), gtx.Sp(18)))
									return u.Label(gtx, guikit.StBody, guikit.ColDim, r.name)
								}),
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									st, col := guikit.StBody, guikit.ColText
									if r.danger {
										st, col = guikit.StBody600, guikit.ColErrorText
									}
									return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											return u.LabelMax(gtx, st, col, 1, r.value)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											if r.code == "" {
												return layout.Dimensions{}
											}
											s, cc := guikit.StMono12, guikit.ColDim
											if r.danger {
												s, cc = guikit.StMono13, guikit.ColErrorText
											} else {
												return layout.Inset{Left: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return u.Label(gtx, s, cc, r.code) })
											}
											return u.Label(gtx, s, cc, r.code)
										}),
									)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return u.Link(gtx, &u.change[i], "Change")
								}),
							)
						})
						if i < len(rows)-1 {
							guikit.FillRect(gtx, image.Rect(0, dims.Size.Y-guikit.Dp(gtx, 1), dims.Size.X, dims.Size.Y), guikit.Over(guikit.ColSidebar, guikit.ColText, 0.05))
						}
						return dims
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
			})
		}),
		guikit.VGap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Warning(gtx, fmt.Sprintf("Installing erases everything on %s. This cannot be undone.", c.Disk))
		}),
	)
}

// --- 8. Installing, and the failure -------------------------------------------

func (u *UI) installing(gtx layout.Context) layout.Dimensions {
	if u.install.failed {
		return u.failed(gtx)
	}
	s := &u.install
	cur := s.step
	phase := float32(u.now().UnixMilli()%1000) / 1000
	return layout.Inset{Top: 44, Bottom: 24, Left: 56, Right: 56}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				o := op.Offset(guikit.Pt(gtx, -44, -30)).Push(gtx.Ops)
				guikit.Glow(gtx, 200, guikit.ColAccent, 0.12)
				o.Pop()
				return guikit.Mark(gtx, 112, true, 1, 1+0.12*guikit.Breath(u.now()))
			}),
			guikit.VGap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return u.Label(gtx, stInstTitle, guikit.ColText, "Installing Isotope")
			}),
			guikit.VGap(6),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return u.Label(gtx, guikit.StBody, guikit.ColDim, "Do not turn off the computer.")
			}),
			guikit.VGap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return layout.Flex{Spacing: layout.SpaceBetween}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.Label(gtx, guikit.StSmall500, guikit.ColText, core.Steps[cur].Name)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								// Nothing until the active system has
								// been installing for 30 seconds.
								if left, ok := u.estimate(); ok {
									return u.Label(gtx, guikit.StSmall, guikit.ColDim, core.RemainingText(left))
								}
								return layout.Dimensions{}
							}),
						)
					}),
					guikit.VGap(8),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return guikit.ProgressBar(gtx, float32(core.Percent(cur))/100)
					}),
				)
			}),
			guikit.VGap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				steps := u.visibleSteps()
				half := (len(steps) + 1) / 2
				col := func(from, to int) layout.Widget {
					return func(gtx layout.Context) layout.Dimensions {
						var kids []layout.FlexChild
						for n := from; n < to; n++ {
							i := steps[n]
							if n > from {
								kids = append(kids, guikit.VGap(8))
							}
							kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.stageRow(gtx, i, cur, phase)
							}))
						}
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
					}
				}
				return layout.Flex{}.Layout(gtx,
					layout.Flexed(1, col(0, half)),
					layout.Rigid(layout.Spacer{Width: 28}.Layout),
					layout.Flexed(1, col(half, len(steps))),
				)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							txt := "Show details"
							if s.show {
								txt = "Hide details"
							}
							return u.details.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								col := guikit.ColDim
								if gtx.Focused(&u.details) {
									col = guikit.ColText
								}
								return u.Label(gtx, guikit.StSmall, col, txt)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if !s.show {
								return layout.Dimensions{}
							}
							return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return u.logBox(gtx, 110)
							})
						}),
					)
				})
			}),
		)
	})
}

// estimate is how long the install has left, once there is one.
func (u *UI) estimate() (time.Duration, bool) {
	s := &u.install
	if s.started.IsZero() || s.activeAt.IsZero() {
		return 0, false
	}
	now := u.now()
	return core.EstimateRemaining(s.step, now.Sub(s.started), now.Sub(s.activeAt), s.encrypt)
}

func (u *UI) stageRow(gtx layout.Context, i, cur int, phase float32) layout.Dimensions {
	st, col := guikit.StSmall, guikit.ColDim
	icon := func(gtx layout.Context) layout.Dimensions { return guikit.RingIcon(gtx, 12) }
	switch {
	case i < cur:
		col = guikit.ColText2
		icon = func(gtx layout.Context) layout.Dimensions { return guikit.CheckIcon(gtx, 12) }
	case i == cur:
		st, col = guikit.StSmall600, guikit.ColText
		icon = func(gtx layout.Context) layout.Dimensions { return guikit.Spinner(gtx, 12, phase) }
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(icon),
		layout.Rigid(layout.Spacer{Width: 10}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.Label(gtx, st, col, core.Steps[i].Name) }),
	)
}

// logBox is the install log, newest line at the bottom.
func (u *UI) logBox(gtx layout.Context, height float32) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	gtx.Constraints.Max.Y = guikit.Dp(gtx, height)
	gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
	b := guikit.Box{Radius: 8, Fill: guikit.ColKeyBox}
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = gtx.Constraints.Max
		return layout.UniformInset(12).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lines := u.install.lines
			return u.logList.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
				return u.LabelMax(gtx, guikit.StMono12, guikit.ColDim, 1, lines[i])
			})
		})
	})
}

func (u *UI) failed(gtx layout.Context) layout.Dimensions {
	s := &u.install
	stage := core.Steps[s.step].Name
	why := ""
	if s.err != nil {
		why = s.err.Error()
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 36, Bottom: 20, Left: 40, Right: 40}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions { return guikit.WarnIcon(gtx, 28, guikit.ColError) }),
							layout.Rigid(layout.Spacer{Width: 14}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.Label(gtx, guikit.StTitle, guikit.ColText, "Isotope could not be installed")
							}),
						)
					}),
					guikit.VGap(10),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.LabelLines(gtx, guikit.StBody, guikit.ColText2, 1.5, fmt.Sprintf("It stopped at \"%s\": %s. Save the log to report it, or restart the computer to try again.", stage, why))
					}),
					guikit.VGap(18),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, guikit.StLabel, guikit.ColText2, "The end of the install log")
					}),
					guikit.VGap(8),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return u.logBox(gtx, float32(gtx.Constraints.Max.Y)/gtx.Metric.PxPerDp)
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.Footer(gtx,
				[]guikit.Action{{Click: &u.saveLog, Kind: guikit.ButtonGhost, Label: "Save the log", Disabled: u.hooks.saveLog == nil}},
				func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return layout.Inset{Left: 8, Right: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return u.LabelMax(gtx, guikit.StTiny, guikit.ColText2, 2, u.savedTo)
					})
				},
				[]guikit.Action{
					{Click: &u.terminal, Kind: guikit.ButtonSecondary, Label: "Open a terminal", Disabled: u.hooks.terminal == nil},
					{Click: &u.restart, Kind: guikit.ButtonPrimary, Label: "Restart"},
				})
		}),
	)
}

// --- 9. Done ------------------------------------------------------------------

func (u *UI) done(gtx layout.Context) layout.Dimensions {
	s := &u.install
	msg := "Remove the installation media, then restart and sign in as " + s.user + "."
	if s.encrypt {
		msg = "Remove the installation media, then restart. You will type your disk passphrase, then sign in as " + s.user + "."
	}
	if s.user == "" {
		msg = "Remove the installation media, then restart."
	}
	var checks []string
	if s.encrypt {
		checks = append(checks, "Encrypted")
	}
	checks = append(checks, "Recovery image ready", "Updates roll back by themselves")
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = gtx.Constraints.Max
			return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Max.X = guikit.Dp(gtx, 520)
				return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						guikit.Glow(gtx, 168, guikit.ColAccent, 0.16)
						o := op.Offset(guikit.Pt(gtx, 18, 18)).Push(gtx.Ops)
						guikit.Mark(gtx, 132, false, 1, 0)
						o.Pop()
						return layout.Dimensions{Size: guikit.Pt(gtx, 168, 168)}
					}),
					guikit.VGap(20),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.Label(gtx, stDoneTitle, guikit.ColText, "Isotope is installed")
					}),
					guikit.VGap(20),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Max.X = guikit.Dp(gtx, 470)
						return u.Centered(gtx, guikit.StLead, guikit.ColText2, 1.55, msg)
					}),
					guikit.VGap(28),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						var kids []layout.FlexChild
						for i, c := range checks {
							c := c
							if i > 0 {
								kids = append(kids, layout.Rigid(layout.Spacer{Width: 28}.Layout))
							}
							kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions { return guikit.CheckIcon(gtx, 12) }),
									layout.Rigid(layout.Spacer{Width: 8}.Layout),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.Label(gtx, guikit.StSmall, guikit.ColDim, c) }),
								)
							}))
						}
						return layout.Flex{}.Layout(gtx, kids...)
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Top: 22, Bottom: 30}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.Button(gtx, &u.restart, guikit.ButtonPrimary, "Restart", true)
				})
			})
		}),
	)
}
