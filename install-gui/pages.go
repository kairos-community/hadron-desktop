package main

import (
	"fmt"
	"image"
	"image/color"
	"strconv"
	"strings"
	"time"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/kairos-community/isotope/install-ui/core"
)

// The window is 920x620 logical pixels, centred. On an output smaller than
// 1000x700 it fills the output with no corners or shadow.
const (
	winW, winH   = 920, 620
	floatW, floa = 1000, 700
	slideTime    = 180 * time.Millisecond
)

// Layout draws one frame.
func (u *UI) Layout(gtx layout.Context) layout.Dimensions {
	if u.scale != 1 {
		gtx.Metric.PxPerDp *= u.scale
		gtx.Metric.PxPerSp *= u.scale
	}
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

	size := gtx.Constraints.Max
	floating := size.X >= dp(gtx, floatW) && size.Y >= dp(gtx, floa)
	win := image.Rectangle{Max: size}
	if floating {
		w, h := dp(gtx, winW), dp(gtx, winH)
		win = image.Rectangle{Min: image.Pt((size.X-w)/2, (size.Y-h)/2)}
		win.Max = win.Min.Add(image.Pt(w, h))
	}
	bg := u.bg.image(size, win, floating, gtx.Metric.PxPerDp)
	paint.NewImageOp(bg).Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)

	rad := 0
	if floating {
		rad = dp(gtx, 16)
	}
	o := op.Offset(win.Min).Push(gtx.Ops)
	wr := image.Rectangle{Max: win.Size()}
	cl := clip.UniformRRect(wr, rad).Push(gtx.Ops)
	fillRect(gtx, wr, colWindow)
	wgtx := gtx
	wgtx.Constraints = layout.Exact(win.Size())
	if u.w == nil && u.page < pInstalling {
		u.blank(wgtx)
	} else if u.page == pDone {
		u.done(wgtx)
	} else {
		layout.Flex{}.Layout(wgtx,
			layout.Rigid(u.sidebar),
			layout.Flexed(1, u.content),
		)
	}
	cl.Pop()
	if floating {
		strokeRRect(gtx, wr, rad, fdp(gtx, 1), over(colWindow, colText, 0.09))
	}
	o.Pop()

	if !u.startedOK {
		u.startedOK = true
		if u.hooks.started != nil {
			u.hooks.started()
		}
	}
	switch {
	case !u.pageAt.IsZero() && u.now().Sub(u.pageAt) < slideTime:
		gtx.Execute(op.InvalidateCmd{})
	case u.page == pWelcome || u.page == pInstalling && !u.install.failed:
		// The breathing mark and the spinner: 20 frames a second is
		// plenty for a slow sine, and spares llvmpipe.
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(50 * time.Millisecond)})
	}
	return layout.Dimensions{Size: size}
}

// blank is the window before core has handed over the wizard.
func (u *UI) blank(gtx layout.Context) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return mark(gtx, 148, false, 1, 0) })
}

// --- sidebar ----------------------------------------------------------------

func (u *UI) sidebar(gtx layout.Context) layout.Dimensions {
	w, h := dp(gtx, 236), gtx.Constraints.Max.Y
	fillRect(gtx, image.Rect(0, 0, w, h), colSidebar)
	fillRect(gtx, image.Rect(w-dp(gtx, 1), 0, w, h), over(colSidebar, colText, 0.06))
	gtx.Constraints = layout.Exact(image.Pt(w, h))
	cur := stepOf(u.page)
	return layout.Inset{Top: 28, Bottom: 28, Left: 22, Right: 22}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return mark(gtx, 30, false, 1, 0) }),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stBrand, colText, "Install Isotope")
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: 28}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				var kids []layout.FlexChild
				for i, name := range stepNames {
					i, name := i, name
					if i > 0 {
						kids = append(kids, layout.Rigid(layout.Spacer{Height: 4}.Layout))
					}
					kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.stepRow(gtx, name, i, cur)
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Dimensions{Size: gtx.Constraints.Min}
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.label(gtx, stTiny, colDim, u.version)
			}),
		)
	})
}

func (u *UI) stepRow(gtx layout.Context, name string, i, cur int) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	st, col := stBody, colDim
	switch {
	case i < cur:
		col = colText2
	case i == cur:
		st, col = stBody600, colText
	}
	m := op.Record(gtx.Ops)
	dims := layout.Inset{Top: 9, Bottom: 9, Left: 10, Right: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				switch {
				case i < cur:
					return checkIcon(gtx, 10)
				case i == cur:
					s := dp(gtx, 10)
					r := dp(gtx, 4)
					fillRRect(gtx, image.Rect(-r, -r, s+r, s+r), s/2+r, over(over(colSidebar, colAccent, 0.12), colAccent, 0.2))
					fillRRect(gtx, image.Rect(0, 0, s, s), s/2, colAccent)
					return layout.Dimensions{Size: image.Pt(s, s)}
				}
				return ringIcon(gtx, 10)
			}),
			layout.Rigid(layout.Spacer{Width: 12}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, st, col, name) }),
		)
	})
	c := m.Stop()
	if i == cur {
		fillRRect(gtx, image.Rectangle{Max: dims.Size}, dp(gtx, 8), over(colSidebar, colAccent, 0.12))
	}
	c.Add(gtx.Ops)
	return dims
}

// --- content ----------------------------------------------------------------

func (u *UI) content(gtx layout.Context) layout.Dimensions {
	gtx.Constraints.Min = gtx.Constraints.Max
	if u.page == pInstalling {
		return u.slide(gtx, u.installing)
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min = gtx.Constraints.Max
			return u.slide(gtx, u.body)
		}),
		layout.Rigid(u.footer),
	)
}

// slide is the page transition: 24 px and a fade over 180 ms.
func (u *UI) slide(gtx layout.Context, w layout.Widget) layout.Dimensions {
	if u.pageAt.IsZero() {
		return w(gtx)
	}
	t := float32(u.now().Sub(u.pageAt)) / float32(slideTime)
	if t >= 1 {
		return w(gtx)
	}
	e := 1 - (1-t)*(1-t)
	defer op.Offset(image.Pt(dp(gtx, 24*(1-e)), 0)).Push(gtx.Ops).Pop()
	defer paint.PushOpacity(gtx.Ops, e).Pop()
	return w(gtx)
}

func (u *UI) body(gtx layout.Context) layout.Dimensions {
	switch u.page {
	case pWelcome:
		return u.welcome(gtx)
	case pKeyboard:
		return u.pad(gtx, u.keyboard)
	case pAccount:
		return u.pad(gtx, u.account)
	case pDisk:
		return u.pad(gtx, u.disk)
	case pEncrypt:
		return u.pad(gtx, u.encryption)
	case pRecovery:
		return u.pad(gtx, u.recovery)
	case pSummary:
		return u.pad(gtx, u.summary)
	}
	return layout.Dimensions{}
}

func (u *UI) pad(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return layout.Inset{Top: 36, Bottom: 20, Left: 40, Right: 40}.Layout(gtx, w)
}

// footer is the bar with Go back and Continue.
func (u *UI) footer(gtx layout.Context) layout.Dimensions {
	m := op.Record(gtx.Ops)
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	dims := layout.Inset{Top: 18, Bottom: 18, Left: 24, Right: 24}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		if u.page == pWelcome {
			return layout.Flex{Spacing: layout.SpaceBetween, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.button(gtx, &u.shutdown, btnGhost, "Shut down", true)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.label(gtx, stSmall, colDim, "About five minutes")
						}),
						layout.Rigid(layout.Spacer{Width: 20}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return u.button(gtx, &u.cont, btnPrimary, "Continue", true)
						}),
					)
				}),
			)
		}
		kind, txt, enabled := btnPrimary, "Continue", true
		switch u.page {
		case pSummary:
			kind, txt = btnDanger, "Erase and install"
		case pRecovery:
			enabled = u.recoveryConfirmed()
		case pAccount:
			enabled = u.passwordsMatch()
		case pDisk:
			enabled = len(u.w.Disks) > 0
		}
		return layout.Flex{Spacing: layout.SpaceStart, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.button(gtx, &u.back, btnSecondary, "Go back", true)
			}),
			layout.Rigid(layout.Spacer{Width: 10}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.button(gtx, &u.cont, kind, txt, enabled)
			}),
		)
	})
	c := m.Stop()
	fillRect(gtx, image.Rectangle{Max: dims.Size}, colFooter)
	fillRect(gtx, image.Rect(0, 0, dims.Size.X, dp(gtx, 1)), over(colFooter, colText, 0.06))
	c.Add(gtx.Ops)
	return dims
}

// heading is a page title and the line under it.
func (u *UI) heading(title, sub string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stTitle, colText, title) }),
			layout.Rigid(layout.Spacer{Height: 6}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, dp(gtx, 580))
				return u.labelLines(gtx, stBody, colDim, 1.5, sub)
			}),
		)
	}
}

// scrolled lets a page that grows (error lines, a larger scale) scroll
// instead of being cut at the footer.
func (u *UI) scrolled(gtx layout.Context, l *widget.List, w layout.Widget) layout.Dimensions {
	return l.List.Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions { return w(gtx) })
}

func vgap(h float32) layout.FlexChild { return layout.Rigid(layout.Spacer{Height: unit.Dp(h)}.Layout) }

// --- 1. Welcome --------------------------------------------------------------

func (u *UI) welcome(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: 40, Bottom: 40, Left: 64, Right: 64}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.W.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, dp(gtx, 560))
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					// The mark lit, as the splash leaves it; its side
					// breathes a little out and back.
					return mark(gtx, 112, true, 1, 0.5*(1+breath(u.now())))
				}),
				vgap(36),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return u.label(gtx, stHero, colText, "Welcome to Isotope")
				}),
				vgap(14),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, dp(gtx, 500))
					return u.labelLines(gtx, stLead, colText2, 1.55, "An immutable Linux desktop, built on Hadron. The system is a read-only image. An update arrives as a whole new image, and a bad one rolls back by itself.")
				}),
				vgap(40),
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
			line, name, sub := colRing, colDim, colDim
			if i == 0 {
				line, name, sub = colAccent, colText, colText2
			}
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					h := dp(gtx, 2)
					fillRect(gtx, image.Rect(0, 0, gtx.Constraints.Max.X, h), line)
					return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, h)}
				}),
				vgap(10),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stCaps, name, spaced(n[0])) }),
				vgap(4),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall, sub, n[1]) }),
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
		layout.Rigid(u.heading("Keyboard and time zone", "The layout applies at once, here and on the disk passphrase prompt at every start.")),
		vgap(22),
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
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stLabel, colText2, "Keyboard layout") }),
		vgap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.input(gtx, u.kbSearch, stBody, 38, "", nil)
		}),
		vgap(10),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return u.pickList(gtx, &u.kbList, u.kbShown, u.kbSel, u.kbRows, func(i int) (string, string) {
				k := u.w.Keyboards[i]
				return k.Description, k.Code()
			})
		}),
		vgap(14),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall, colDim, "Try it") }),
		vgap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.input(gtx, u.kbTry, stBody, 38, "", nil) }),
	)
}

// pickList is the list of layouts or zones: the selected row in the accent
// colour, an optional code on the right.
func (u *UI) pickList(gtx layout.Context, l *widget.List, shown []int, sel int, rows []rowClick, text func(int) (string, string)) layout.Dimensions {
	gtx.Constraints.Min = gtx.Constraints.Max
	return panel(gtx, 10, layout.UniformInset(6), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = gtx.Constraints.Max
		if len(shown) == 0 {
			return layout.Inset{Top: 9, Left: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.label(gtx, stBody, colDim, "Nothing matches.")
			})
		}
		return l.List.Layout(gtx, len(shown), func(gtx layout.Context, n int) layout.Dimensions {
			i := shown[n]
			name, code := text(i)
			on := i == sel
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Bottom: 2}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return rows[i].layout(gtx, func(gtx layout.Context) layout.Dimensions {
					m := op.Record(gtx.Ops)
					dims := layout.Inset{Top: 9, Bottom: 9, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						fg, st, cc := colText2, stBody, colDim
						if on {
							fg, st, cc = colOnAccent, stBody600, colOnAccent
						}
						return layout.Flex{Spacing: layout.SpaceBetween, Alignment: layout.Middle}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								gtx.Constraints.Min.X = 0
								return widget.Label{MaxLines: 1}.Layout(gtx, u.shaper, st.font(), st.size, name, material(gtx, fg))
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if code == "" {
									return layout.Dimensions{}
								}
								return layout.Inset{Left: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return u.label(gtx, stMono12, cc, code)
								})
							}),
						)
					})
					c := m.Stop()
					switch {
					case on:
						fillRRect(gtx, image.Rectangle{Max: dims.Size}, dp(gtx, 6), colAccent)
					case rows[i].c.Hovered():
						fillRRect(gtx, image.Rectangle{Max: dims.Size}, dp(gtx, 6), over(colSidebar, colText, 0.05))
					}
					c.Add(gtx.Ops)
					return dims
				})
			})
		})
	})
}

func (u *UI) zoneColumn(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stLabel, colText2, "Time zone") }),
		vgap(10),
		// The map, or while a city is being typed the zones it matches.
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if u.zoneFiltering() || len(u.zoneCoords) == 0 {
				return u.pickList(gtx, &u.tzList, u.tzShown, u.tzSel, u.tzRows, func(i int) (string, string) {
					return strings.ReplaceAll(u.w.Zones[i], "_", " "), ""
				})
			}
			gtx.Constraints.Min = gtx.Constraints.Max
			return panel(gtx, 10, layout.Inset{}, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = gtx.Constraints.Max
				defer clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Max}, dp(gtx, 10)).Push(gtx.Ops).Pop()
				return u.worldMap(gtx)
			})
		}),
		vgap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.input(gtx, u.tzField, stBody, 38, "", func(gtx layout.Context) layout.Dimensions {
				return u.label(gtx, stSmall, colDim, u.zoneClock())
			})
		}),
		vgap(10),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if u.tzField.err != "" {
				return u.label(gtx, stSmall, colError, u.tzField.err)
			}
			hint := "Found from this computer's clock. Click the map or type a city."
			if len(u.zoneCoords) == 0 {
				hint = "Found from this computer's clock. Type a city to change it."
			}
			return u.labelLines(gtx, stSmall, colDim, 1.4, hint)
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
			return u.scrolled(gtx, &u.acctList, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(u.heading("Computer and account", "This account is the computer's administrator: it installs updates and extensions.")),
					vgap(16),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.labelled(gtx, "Computer name", u.host, "", func(gtx layout.Context) layout.Dimensions {
							return u.input(gtx, u.host, stBody, 38, "", nil)
						})
					}),
					vgap(16),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.labelled(gtx, "User name", u.user, "Lowercase letters, digits, - or _, up to 32 characters.", func(gtx layout.Context) layout.Dimensions {
							return u.input(gtx, u.user, stBody, 38, "", nil)
						})
					}),
					vgap(16),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						hint := ""
						if u.pw2.ed.Len() > 0 && !u.passwordsMatch() {
							hint = "The passwords do not match yet."
						}
						return u.twoCols(gtx,
							func(gtx layout.Context) layout.Dimensions {
								return u.labelled(gtx, "Password", u.pw, "", func(gtx layout.Context) layout.Dimensions {
									return u.input(gtx, u.pw, stBody, 38, "", nil)
								})
							},
							func(gtx layout.Context) layout.Dimensions {
								return u.labelled(gtx, "Type it again", u.pw2, hint, func(gtx layout.Context) layout.Dimensions {
									return u.input(gtx, u.pw2, stBody, 38, "", nil)
								})
							})
					}),
					vgap(20),
					layout.Rigid(u.githubBox),
				)
			})
		}),
	)
}

func (u *UI) twoCols(gtx layout.Context, a, b layout.Widget) layout.Dimensions {
	return layout.Flex{}.Layout(gtx,
		layout.Flexed(1, a),
		layout.Rigid(layout.Spacer{Width: 14}.Layout),
		layout.Flexed(1, b),
	)
}

// avatar is the account page's left column: the user's initial, name and
// computer name, as typed.
func (u *UI) avatar(gtx layout.Context) layout.Dimensions {
	gtx.Constraints = layout.Exact(image.Pt(dp(gtx, 150), gtx.Constraints.Max.Y))
	name := strings.TrimSpace(u.user.ed.Text())
	host := strings.TrimSpace(u.host.ed.Text())
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
				s := dp(gtx, 104)
				b := box{fill: colField, border: over(colWindow, colText, 0.08), bw: 1, radius: 52}
				b.paint(gtx, image.Pt(s, s))
				gtx.Constraints = layout.Exact(image.Pt(s, s))
				return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return u.label(gtx, stAvatar, colGlow, initial)
				})
			}),
			vgap(10),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				if name == "" {
					name = " "
				}
				return widget.Label{MaxLines: 1}.Layout(gtx, u.shaper, stSmall.font(), stSmall.size, name, material(gtx, colText2))
			}),
			vgap(10),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return widget.Label{MaxLines: 1}.Layout(gtx, u.shaper, stMono12.font(), stMono12.size, "@"+host, material(gtx, colDim))
			}),
		)
	})
}

func (u *UI) githubBox(gtx layout.Context) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return panel(gtx, 10, layout.Inset{Top: 14, Bottom: 14, Left: 16, Right: 16}, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return keyIcon(gtx, 16) }),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stLabel, colText2, "SSH keys from GitHub ")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall, colDim, "(optional)") }),
				)
			}),
			vgap(10),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.input(gtx, u.gh, stMono, 38, "github.com/", nil)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if u.gh.err != "" {
					return layout.Inset{Top: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stTiny, colError, u.gh.err)
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
								return checkIcon(gtx, 12)
							}
							if txt == "Checking GitHub" {
								return spinner(gtx, 12, float32(u.now().UnixMilli()%1000)/1000)
							}
							return warnIcon(gtx, 13, colWarn)
						}),
						layout.Rigid(layout.Spacer{Width: 6}.Layout),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							col := colText2
							if good {
								col = colGlow
							}
							return u.label(gtx, stSmall, col, txt)
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
			layout.Rigid(u.heading("Where should Isotope go?", "No disk was found. Connect one, then rescan.")),
			vgap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.button(gtx, &u.rescan, btnSecondary, "Rescan", true)
			}),
		)
	}
	info := u.info(u.diskSel)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.heading("Where should Isotope go?", "Isotope uses the whole disk. Everything on the disk you pick is erased.")),
		vgap(22),
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
		vgap(14),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.warning(gtx, info.ErasureWarning())
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
	return u.card(gtx, &u.diskCards[i], on, layout.UniformInset(18), func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return diskIcon(gtx, 48, on, usb) }),
			vgap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return widget.Label{MaxLines: 1}.Layout(gtx, u.shaper, stCard.font(), stCard.size, u.diskTitle(i), material(gtx, colText))
			}),
			vgap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall, colText2, u.diskLine(i)) }),
			vgap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stMono12, colDim, d.Path) }),
		)
	})
}

// segColors are the disk bar's partitions, in turn; free space is the
// field colour.
var segColors = []func() color.NRGBA{
	func() color.NRGBA { return colRing },
	func() color.NRGBA { return rgb(0x4a4857) },
	func() color.NRGBA { return rgb(0x6e6a80) },
	func() color.NRGBA { return rgb(0x3f3d4b) },
}

// diskBar is what is on the chosen disk now: one segment per partition, and
// their names and sizes under it.
func (u *UI) diskBar(gtx layout.Context, i int) layout.Dimensions {
	info := u.info(i)
	segs := info.Segments()
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return panel(gtx, 12, layout.Inset{Top: 16, Bottom: 16, Left: 18, Right: 18}, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Spacing: layout.SpaceBetween}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stLabel, colText2, "On "+u.w.Disks[i].Path+" now")
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stSmall, colDim, core.HumanSize(info.SizeBytes))
					}),
				)
			}),
			vgap(12),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				w, h := gtx.Constraints.Max.X, dp(gtx, 14)
				defer clip.UniformRRect(image.Rect(0, 0, w, h), h/2).Push(gtx.Ops).Pop()
				if len(segs) == 0 {
					fillRect(gtx, image.Rect(0, 0, w, h), colField)
					return layout.Dimensions{Size: image.Pt(w, h)}
				}
				gap := dp(gtx, 2)
				avail := w - gap*(len(segs)-1)
				x := 0
				for n, sg := range segs {
					sw := int(float64(avail) * float64(sg.SizeBytes) / float64(info.SizeBytes))
					if sw < dp(gtx, 3) {
						sw = dp(gtx, 3)
					}
					if n == len(segs)-1 || x+sw > w {
						sw = w - x
					}
					c := colField
					if !sg.Free {
						c = segColors[n%len(segColors)]()
					}
					fillRect(gtx, image.Rect(x, 0, x+sw, h), c)
					x += sw + gap
				}
				return layout.Dimensions{Size: image.Pt(w, h)}
			}),
			vgap(12),
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
				return u.labelLines(gtx, stTiny, colDim, 1.5, strings.Join(parts, "     "))
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
			layout.Rigid(u.heading("Create a recovery key?", tpmText)),
			vgap(18),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.twoCols(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return u.choiceCard(gtx, &u.encNo, !u.recKey, "No, the TPM only", "Nothing to keep safe.")
					},
					func(gtx layout.Context) layout.Dimensions {
						return u.choiceCard(gtx, &u.encYes, u.recKey, "Create a recovery key", "You see it once, on the next screen.")
					})
			}),
			layout.Rigid(u.encError),
		)
	}
	kids := []layout.FlexChild{
		layout.Rigid(u.heading("Encrypt your files?", "With encryption, nobody can read your files from a lost or stolen disk. You type a passphrase every time the computer starts.")),
		vgap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.twoCols(gtx,
				func(gtx layout.Context) layout.Dimensions {
					return u.choiceCard(gtx, &u.encNo, !u.encOn, "Don't encrypt", "Starts without a passphrase.")
				},
				func(gtx layout.Context) layout.Dimensions {
					return u.choiceCard(gtx, &u.encYes, u.encOn, "Encrypt my files", "A passphrase at every start.")
				})
		}),
	}
	if u.encOn {
		kids = append(kids,
			vgap(18),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.twoCols(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return u.labelled(gtx, "Disk passphrase", u.pp, "", func(gtx layout.Context) layout.Dimensions {
							return u.input(gtx, u.pp, stBody, 38, "", nil)
						})
					},
					func(gtx layout.Context) layout.Dimensions {
						return u.labelled(gtx, "Type it again", u.pp2, "", func(gtx layout.Context) layout.Dimensions {
							return u.input(gtx, u.pp2, stBody, 38, "", nil)
						})
					})
			}),
			vgap(18),
			layout.Rigid(u.strength),
			vgap(18),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return u.label(gtx, stTiny, colDim, fmt.Sprintf("%d characters or more. A few words work well.", core.MinPassphraseLen))
			}),
			vgap(18),
			layout.Rigid(u.recoveryBox),
		)
	}
	kids = append(kids, layout.Rigid(u.encError))
	return u.scrolled(gtx, &u.encList, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
	})
}

func (u *UI) encError(gtx layout.Context) layout.Dimensions {
	if u.encErr == "" {
		return layout.Dimensions{}
	}
	return layout.Inset{Top: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return u.label(gtx, stSmall, colError, u.encErr)
	})
}

func (u *UI) choiceCard(gtx layout.Context, c *widget.Clickable, on bool, title, sub string) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return u.card(gtx, c, on, layout.Inset{Top: 16, Bottom: 16, Left: 18, Right: 18}, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 2}.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return radio(gtx, on) })
			}),
			layout.Rigid(layout.Spacer{Width: 14}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stCard, colText, title) }),
					vgap(4),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall, colDim, sub) }),
				)
			}),
		)
	})
}

// strength is the meter under the passphrase: core.PassphraseStrength in
// four bars and its words.
func (u *UI) strength(gtx layout.Context) layout.Dimensions {
	s := core.PassphraseStrength(u.pp.ed.Text())
	bars, col, txtCol := 0, colAccent, colGlow
	switch {
	case s == "":
	case strings.HasPrefix(s, "too short"):
		bars, col, txtCol = 1, colError, colErrorText
	case strings.HasPrefix(s, "weak"):
		bars, col, txtCol = 2, colError, colErrorText
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
			gap := dp(gtx, 4)
			bw := (w - 3*gap) / 4
			h := dp(gtx, 5)
			for i := 0; i < 4; i++ {
				c := colField
				if i < bars {
					c = col
				}
				x := i * (bw + gap)
				fillRRect(gtx, image.Rect(x, 0, x+bw, h), dp(gtx, 3), c)
			}
			return layout.Dimensions{Size: image.Pt(w, h)}
		}),
		layout.Rigid(layout.Spacer{Width: 12}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints = layout.Exact(image.Pt(dp(gtx, 240), gtx.Sp(16)))
			return u.label(gtx, stSmall, txtCol, s)
		}),
	)
}

func (u *UI) recoveryBox(gtx layout.Context) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	b := box{radius: 10, fill: colSidebar, border: over(colSidebar, colText, 0.06), bw: 1}
	if gtx.Focused(&u.recBox) {
		b.border, b.ring, b.ringW = colAccent, focusRing, 3
	}
	return u.recBox.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return b.layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Inset{Top: 14, Bottom: 14, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Top: 1}.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return checkbox(gtx, u.recKey) })
					}),
					layout.Rigid(layout.Spacer{Width: 12}.Layout),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.label(gtx, stBody600, colText, "Also create a recovery key")
							}),
							vgap(4),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.labelLines(gtx, stSmall, colDim, 1.45, "A second way in if you forget the passphrase. You see it once, on the next screen.")
							}),
						)
					}),
				)
			})
		})
	})
}

// --- 6. Recovery key ---------------------------------------------------------

func (u *UI) recovery(gtx layout.Context) layout.Dimensions {
	groups := core.RecoveryKeyGroups(u.w.Choice.RecoveryKeyValue)
	ask := u.w.ConfirmGroup
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(u.heading("Your recovery key", "Write it down or take a photo of it now. It is shown only this once and saved nowhere.")),
		vgap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			b := box{radius: 14, fill: colKeyBox, border: over(colKeyBox, colGlow, 0.25), bw: 1}
			return b.layout(gtx, func(gtx layout.Context) layout.Dimensions {
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
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, row(0), vgap(16), row(1))
				})
			})
		}),
		vgap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.label(gtx, stTiny, colDim, "Only these letters appear: "+strings.Join(strings.Split(core.Modhex, ""), " "))
		}),
		vgap(22),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.End}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Max.X = dp(gtx, 280)
					return u.labelled(gtx, fmt.Sprintf("To check you saved it, type group %d", ask+1), u.rk, "", func(gtx layout.Context) layout.Dimensions {
						return u.input(gtx, u.rk, stMonoField, 40, "", nil)
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
	b := box{radius: 8, border: over(colWindow, colText, 0.1), bw: 1}
	if gtx.Focused(&u.qrButton) {
		b.border, b.ring, b.ringW = colAccent, focusRing, 3
	}
	return u.qrButton.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.Y = dp(gtx, 40)
		return b.layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 14, Right: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return layout.W.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.Y = 0
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return qrGlyph(gtx, 14) }),
						layout.Rigid(layout.Spacer{Width: 8}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall500, colText2, txt) }),
					)
				})
			})
		})
	})
}

func (u *UI) keyGroup(gtx layout.Context, i int, g string, asked bool) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.label(gtx, stMonoIndex, colDim, fmt.Sprint(i+1))
		}),
		vgap(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			col := colGlow
			if asked {
				col = colText
			}
			dims := u.label(gtx, stMonoKey, col, g)
			if asked {
				y := dims.Size.Y + dp(gtx, 4)
				fillRect(gtx, image.Rect(0, y, dims.Size.X, y+dp(gtx, 1)), colAccent)
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
		layout.Rigid(u.heading("Ready to install", "Check what you picked. Click Change to go back to it.")),
		vgap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return panel(gtx, 12, layout.Inset{}, func(gtx layout.Context) layout.Dimensions {
				var kids []layout.FlexChild
				for i, r := range rows {
					i, r := i, r
					kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						dims := layout.Inset{Top: 12, Bottom: 12, Left: 18, Right: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									gtx.Constraints = layout.Exact(image.Pt(dp(gtx, 152), gtx.Sp(18)))
									return u.label(gtx, stBody, colDim, r.name)
								}),
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									st, col := stBody, colText
									if r.danger {
										st, col = stBody600, colErrorText
									}
									return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											return widget.Label{MaxLines: 1}.Layout(gtx, u.shaper, st.font(), st.size, r.value, material(gtx, col))
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											if r.code == "" {
												return layout.Dimensions{}
											}
											s, cc := stMono12, colDim
											if r.danger {
												s, cc = stMono13, colErrorText
											} else {
												return layout.Inset{Left: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions { return u.label(gtx, s, cc, r.code) })
											}
											return u.label(gtx, s, cc, r.code)
										}),
									)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return u.link(gtx, &u.change[i], "Change")
								}),
							)
						})
						if i < len(rows)-1 {
							fillRect(gtx, image.Rect(0, dims.Size.Y-dp(gtx, 1), dims.Size.X, dims.Size.Y), over(colSidebar, colText, 0.05))
						}
						return dims
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
			})
		}),
		vgap(18),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.warning(gtx, fmt.Sprintf("Installing erases everything on %s. This cannot be undone.", c.Disk))
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
				o := op.Offset(pt(gtx, -44, -30)).Push(gtx.Ops)
				glow(gtx, 200, colAccent, 0.12)
				o.Pop()
				return mark(gtx, 112, true, 1, 1+0.12*breath(u.now()))
			}),
			vgap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return u.label(gtx, stInstTitle, colText, "Installing Isotope")
			}),
			vgap(6),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = 0
				return u.label(gtx, stBody, colDim, "Do not turn off the computer.")
			}),
			vgap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return layout.Flex{Spacing: layout.SpaceBetween}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.label(gtx, stSmall500, colText, core.Steps[cur].Name)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								// Nothing until the active system has
								// been installing for 30 seconds.
								if left, ok := u.estimate(); ok {
									return u.label(gtx, stSmall, colDim, core.RemainingText(left))
								}
								return layout.Dimensions{}
							}),
						)
					}),
					vgap(8),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return progressBar(gtx, float32(core.Percent(cur))/100)
					}),
				)
			}),
			vgap(22),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				steps := u.visibleSteps()
				half := (len(steps) + 1) / 2
				col := func(from, to int) layout.Widget {
					return func(gtx layout.Context) layout.Dimensions {
						var kids []layout.FlexChild
						for n := from; n < to; n++ {
							i := steps[n]
							if n > from {
								kids = append(kids, vgap(8))
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
								col := colDim
								if gtx.Focused(&u.details) {
									col = colText
								}
								return u.label(gtx, stSmall, col, txt)
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
	st, col := stSmall, colDim
	icon := func(gtx layout.Context) layout.Dimensions { return ringIcon(gtx, 12) }
	switch {
	case i < cur:
		col = colText2
		icon = func(gtx layout.Context) layout.Dimensions { return checkIcon(gtx, 12) }
	case i == cur:
		st, col = stSmall600, colText
		icon = func(gtx layout.Context) layout.Dimensions { return spinner(gtx, 12, phase) }
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(icon),
		layout.Rigid(layout.Spacer{Width: 10}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, st, col, core.Steps[i].Name) }),
	)
}

// progressBar is the 8 px bar: green to glow, as the mocks' gradient.
func progressBar(gtx layout.Context, f float32) layout.Dimensions {
	w, h := gtx.Constraints.Max.X, dp(gtx, 8)
	r := dp(gtx, 4)
	fillRRect(gtx, image.Rect(0, 0, w, h), r, colField)
	fw := int(float32(w) * f)
	if fw > 0 {
		cl := clip.UniformRRect(image.Rect(0, 0, fw, h), r).Push(gtx.Ops)
		paint.LinearGradientOp{
			Stop1: f32.Pt(0, 0), Color1: colCheck,
			Stop2: f32.Pt(float32(fw)*0.7, 0), Color2: colAccent,
		}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		cl.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, h)}
}

// logBox is the install log, newest line at the bottom.
func (u *UI) logBox(gtx layout.Context, height float32) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	gtx.Constraints.Max.Y = dp(gtx, height)
	gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
	b := box{radius: 8, fill: colKeyBox}
	return b.layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = gtx.Constraints.Max
		return layout.UniformInset(12).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			lines := u.install.lines
			return u.logList.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
				return widget.Label{MaxLines: 1}.Layout(gtx, u.shaper, stMono12.font(), stMono12.size, lines[i], material(gtx, colDim))
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
							layout.Rigid(func(gtx layout.Context) layout.Dimensions { return warnIcon(gtx, 28, colError) }),
							layout.Rigid(layout.Spacer{Width: 14}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return u.label(gtx, stTitle, colText, "Isotope could not be installed")
							}),
						)
					}),
					vgap(10),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.labelLines(gtx, stBody, colText2, 1.5, fmt.Sprintf("It stopped at \"%s\": %s. Save the log to report it, or restart the computer to try again.", stage, why))
					}),
					vgap(18),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stLabel, colText2, "The end of the install log")
					}),
					vgap(8),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return u.logBox(gtx, float32(gtx.Constraints.Max.Y)/gtx.Metric.PxPerDp)
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			m := op.Record(gtx.Ops)
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			dims := layout.Inset{Top: 18, Bottom: 18, Left: 24, Right: 24}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.button(gtx, &u.saveLog, btnGhost, "Save the log", u.hooks.saveLog != nil)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return layout.Inset{Left: 8, Right: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return widget.Label{MaxLines: 2}.Layout(gtx, u.shaper, stTiny.font(), stTiny.size, u.savedTo, material(gtx, colText2))
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.button(gtx, &u.terminal, btnSecondary, "Open a terminal", u.hooks.terminal != nil)
					}),
					layout.Rigid(layout.Spacer{Width: 10}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.button(gtx, &u.restart, btnPrimary, "Restart", true)
					}),
				)
			})
			c := m.Stop()
			fillRect(gtx, image.Rectangle{Max: dims.Size}, colFooter)
			fillRect(gtx, image.Rect(0, 0, dims.Size.X, dp(gtx, 1)), over(colFooter, colText, 0.06))
			c.Add(gtx.Ops)
			return dims
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
				gtx.Constraints.Max.X = dp(gtx, 520)
				return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						glow(gtx, 168, colAccent, 0.16)
						o := op.Offset(pt(gtx, 18, 18)).Push(gtx.Ops)
						mark(gtx, 132, false, 1, 0)
						o.Pop()
						return layout.Dimensions{Size: pt(gtx, 168, 168)}
					}),
					vgap(20),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return u.label(gtx, stDoneTitle, colText, "Isotope is installed")
					}),
					vgap(20),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Max.X = dp(gtx, 470)
						return u.centered(gtx, stLead, colText2, 1.55, msg)
					}),
					vgap(28),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						var kids []layout.FlexChild
						for i, c := range checks {
							c := c
							if i > 0 {
								kids = append(kids, layout.Rigid(layout.Spacer{Width: 28}.Layout))
							}
							kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions { return checkIcon(gtx, 12) }),
									layout.Rigid(layout.Spacer{Width: 8}.Layout),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions { return u.label(gtx, stSmall, colDim, c) }),
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
					return u.button(gtx, &u.restart, btnPrimary, "Restart", true)
				})
			})
		}),
	)
}
