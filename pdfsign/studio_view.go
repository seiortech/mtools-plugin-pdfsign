package pdfsign

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/seiortech/mtools-sdk/ui/components"
	"github.com/seiortech/mtools-sdk/ui/theme"
)

// StudioMode represents drawing vs photo tab in signature studio.
const (
	StudioModeDraw  = 0
	StudioModePhoto = 1
)

var (
	ColorInkBlack = color.RGBA{R: 20, G: 20, B: 20, A: 255}
	ColorInkBlue  = color.RGBA{R: 20, G: 50, B: 180, A: 255}
	ColorInkRed   = color.RGBA{R: 190, G: 25, B: 25, A: 255}
)

// StudioState manages the interactive signature creation modal.
type StudioState struct {
	Mode         int // StudioModeDraw or StudioModePhoto

	// Drawing Canvas
	Strokes      []Stroke
	CurrentStroke *Stroke
	ActiveColor  color.RGBA
	ActiveWidth  float64
	IsEraser     bool
	CanvasW      int
	CanvasH      int
	DrawnBitmap  *ui.Bitmap
	DrawnImage   *image.NRGBA

	// Photo Background Removal
	PhotoPath    string
	PhotoSource  image.Image
	Threshold    float64
	Contrast     float64
	InkMode      InkColorMode
	PhotoBitmap  *ui.Bitmap
	PhotoProcessed image.Image
	lastThresh   float64
	lastContrast float64
	lastInkMode  InkColorMode
	lastSource   image.Image
	PhotoError   string

	// Vault Limit Warning
	VaultNotice  string
}

// NewStudioState initializes StudioState with defaults.
func NewStudioState() *StudioState {
	s := &StudioState{
		Mode:        StudioModeDraw,
		ActiveColor: ColorInkBlack,
		ActiveWidth: 3.0,
		CanvasW:     560,
		CanvasH:     240,
		Threshold:   0.80,
		Contrast:    1.5,
		InkMode:     InkBlack,
	}
	s.redrawStrokes()
	return s
}

func (s *StudioState) redrawStrokes() {
	allStrokes := make([]Stroke, len(s.Strokes))
	copy(allStrokes, s.Strokes)
	if s.CurrentStroke != nil && len(s.CurrentStroke.Points) > 0 {
		allStrokes = append(allStrokes, *s.CurrentStroke)
	}

	img := RenderStrokesToImage(s.CanvasW, s.CanvasH, allStrokes)
	s.DrawnImage = img
	s.DrawnBitmap = ui.NewBitmap(img)
}

func (s *StudioState) processPhoto() {
	if s.PhotoSource == nil {
		s.PhotoBitmap = nil
		s.PhotoProcessed = nil
		s.lastSource = nil
		return
	}
	if s.PhotoBitmap != nil && s.PhotoSource == s.lastSource &&
		s.Threshold == s.lastThresh && s.Contrast == s.lastContrast && s.InkMode == s.lastInkMode {
		return
	}
	opts := PhotoProcessOptions{
		Threshold: s.Threshold,
		Contrast:  s.Contrast,
		InkMode:   s.InkMode,
		Crop:      DefaultNormalizedRect(),
	}
	img := RemoveBackground(s.PhotoSource, opts)
	s.PhotoProcessed = img
	s.PhotoBitmap = ui.NewBitmap(img)
	s.lastSource = s.PhotoSource
	s.lastThresh = s.Threshold
	s.lastContrast = s.Contrast
	s.lastInkMode = s.InkMode
}

// ResultImage returns the trimmed, transparent signature ready for placement.
func (s *StudioState) ResultImage() (image.Image, error) {
	if s.Mode == StudioModeDraw {
		if len(s.Strokes) == 0 {
			return nil, fmt.Errorf("drawing canvas is empty")
		}
		trimmed := AutoTrimAlphaBounds(s.DrawnImage, 10)
		return trimmed, nil
	}

	if s.PhotoSource == nil {
		return nil, fmt.Errorf("no photo uploaded")
	}
	opts := PhotoProcessOptions{
		Threshold: s.Threshold,
		Contrast:  s.Contrast,
		InkMode:   s.InkMode,
		Crop:      DefaultNormalizedRect(),
	}
	processed := RemoveBackground(s.PhotoSource, opts)
	trimmed := AutoTrimAlphaBounds(processed, 10)
	return trimmed, nil
}

// RenderStudioDialog renders the signature studio modal.
func (p *Plugin) renderStudioDialog(c *ui.Context) {
	st := p.state.StudioState
	if st == nil {
		return
	}

	ui.DialogBase(c.Key("signature-studio-dialog"), &p.state.StudioOpen, func(_ ui.Element, panel ui.Element) {
		panel.Width(640).Padding(24).Gap(16).
			Background(theme.Paper).Border(4, theme.Ink).Radius(0).
			Shadow(8, 8, 0, 0, theme.Ink)

		// Header
		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Gap(2).Children(func() {
				ui.Text(c, p.t("pdfsign.studio_title")).FontSize(20).FontWeight(700).Role(ui.RoleHeading).Level(2)
				ui.Text(c, p.t("pdfsign.studio_subtitle")).FontSize(12).TextColor(theme.InkMuted)
			})
			closeBtn := ui.ButtonBase(c.Key("studio-close-btn")).
				Width(32).Height(32).Radius(0).
				Background(theme.Paper).Border(2, theme.Ink).
				Justify(ui.Center).AlignItems(ui.Center).
				Label("Close").Tooltip("Close")
			if closeBtn.Hovered() {
				closeBtn.Background(theme.SurfaceHover)
			}
			if closeBtn.Clicked() {
				p.state.StudioOpen = false
				c.Invalidate()
			}
			closeBtn.Children(func() {
				ui.Icon(c, components.IconClose).Size(14, 14).TextColor(theme.Ink)
			})
		})

		// Tab Switcher: Draw vs Photo (Bauhaus block tabs with full keyboard navigation)
		components.Tabs(c, "studio-mode-tabs", &st.Mode, p.t("pdfsign.tab_draw"), p.t("pdfsign.tab_photo"))

		// Studio Body
		if st.Mode == StudioModeDraw {
			p.renderDrawStudio(c, st)
		} else {
			p.renderPhotoStudio(c, st)
		}

		if st.VaultNotice != "" {
			ui.Text(c, st.VaultNotice).FontSize(12).TextColor(theme.Danger)
		}

		// Footer Actions
		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
			components.SecondaryButton(c, p.t("pdfsign.btn_cancel"), func() {
				p.state.StudioOpen = false
				c.Invalidate()
			}).Height(40)

			ui.Row(c).Gap(8).Children(func() {
				// Save to Vault & Place
				components.SecondaryButton(c, p.t("pdfsign.btn_save_vault"), func() {
					img, err := st.ResultImage()
					if err != nil {
						st.VaultNotice = err.Error()
						c.Invalidate()
						return
					}

					// Save into local vault
					vault := p.Vault()
					if vault.Count() >= MaxSavedSignatures {
						st.VaultNotice = p.t("pdfsign.vault_limit_reached")
						c.Invalidate()
						return
					}

					saved, err := vault.SaveImage("Signature "+fmt.Sprintf("%d", vault.Count()+1), "drawn", img)
					if err != nil {
						st.VaultNotice = err.Error()
						c.Invalidate()
						return
					}

					p.placeSignatureOnActivePage(img, saved.ID)
					p.state.StudioOpen = false
					c.Invalidate()
				}).Height(40)

				// Place Directly (without saving to vault)
				components.PrimaryButton(c.Key("studio-place-btn"), p.t("pdfsign.btn_place"), func() {
					img, err := st.ResultImage()
					if err != nil {
						st.VaultNotice = err.Error()
						c.Invalidate()
						return
					}
					p.placeSignatureOnActivePage(img, "")
					p.state.StudioOpen = false
					c.Invalidate()
				}).Height(40).FocusRingColor(theme.Paper)
			})
		})
	})
}

// renderDrawStudio renders the drawing canvas and pen controls.
func (p *Plugin) renderDrawStudio(c *ui.Context, st *StudioState) {
	ui.Column(c).FillWidth().Gap(12).Children(func() {
		// Toolbar: Color pickers, width, eraser, undo, clear
		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
			// Colors
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Text(c, p.t("pdfsign.ink_color")).FontSize(12).TextColor(theme.InkMuted)

				// Black
				blackBtn := components.SecondaryButton(c, "", func() {
					st.ActiveColor = ColorInkBlack
					st.IsEraser = false
					c.Invalidate()
				}).Width(24).Height(24).Background(theme.Ink)
				if !st.IsEraser && st.ActiveColor == ColorInkBlack {
					blackBtn.Border(2, theme.Accent)
				}

				// Blue
				blueBtn := components.SecondaryButton(c, "", func() {
					st.ActiveColor = ColorInkBlue
					st.IsEraser = false
					c.Invalidate()
				}).Width(24).Height(24).Background(ui.Hex("#1432B4"))
				if !st.IsEraser && st.ActiveColor == ColorInkBlue {
					blueBtn.Border(2, theme.Accent)
				}

				// Red
				redBtn := components.SecondaryButton(c, "", func() {
					st.ActiveColor = ColorInkRed
					st.IsEraser = false
					c.Invalidate()
				}).Width(24).Height(24).Background(theme.Danger)
				if !st.IsEraser && st.ActiveColor == ColorInkRed {
					redBtn.Border(2, theme.Accent)
				}
			})

			// Tools: Pen / Eraser
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				eraserBtn := components.SecondaryButton(c, p.t("pdfsign.tool_eraser"), func() {
					st.IsEraser = !st.IsEraser
					c.Invalidate()
				}).Height(30)
				if st.IsEraser {
					eraserBtn.Background(theme.Accent).TextColor(theme.OnField).Border(2, theme.Ink).FocusRingColor(theme.Paper)
				}

				components.SecondaryButton(c, p.t("pdfsign.tool_undo"), func() {
					if len(st.Strokes) > 0 {
						st.Strokes = st.Strokes[:len(st.Strokes)-1]
						st.redrawStrokes()
						c.Invalidate()
					}
				}).Height(30).Disabled(len(st.Strokes) == 0)

				components.SecondaryButton(c, p.t("pdfsign.tool_clear"), func() {
					st.Strokes = nil
					st.CurrentStroke = nil
					st.redrawStrokes()
					c.Invalidate()
				}).Height(30).Disabled(len(st.Strokes) == 0)
			})
		})

		// Interactive Canvas Surface
		canvasBox := ui.Box(c.Key("draw-canvas")).
			Width(float32(st.CanvasW)).Height(float32(st.CanvasH)).
			Background(theme.Paper).Border(2, theme.Ink).Radius(0)

		// Handle mouse inputs directly on the canvas element
		canvasBox.HandleInput(func(ev ui.InputEvent) bool {
			x := float64(ev.X)
			y := float64(ev.Y)

			switch ev.Kind {
			case ui.InputPointerDown:
				st.CurrentStroke = &Stroke{
					Points: []Point{{X: x, Y: y}},
					Width:  st.ActiveWidth,
					Color:  st.ActiveColor,
					Eraser: st.IsEraser,
				}
				st.redrawStrokes()
				c.Invalidate()
				return true

			case ui.InputPointerMove:
				if st.CurrentStroke != nil {
					st.CurrentStroke.Points = append(st.CurrentStroke.Points, Point{X: x, Y: y})
					st.redrawStrokes()
					c.Invalidate()
					return true
				}

			case ui.InputPointerUp:
				if st.CurrentStroke != nil {
					st.CurrentStroke.Points = append(st.CurrentStroke.Points, Point{X: x, Y: y})
					st.Strokes = append(st.Strokes, *st.CurrentStroke)
					st.CurrentStroke = nil
					st.redrawStrokes()
					c.Invalidate()
					return true
				}
			}
			return false
		})

		// Draw bitmap onto canvas
		if st.DrawnBitmap != nil {
			canvasBox.Children(func() {
				ui.Image(c, st.DrawnBitmap).Width(float32(st.CanvasW)).Height(float32(st.CanvasH))
			})
		}
	})
}

// renderPhotoStudio renders photo upload, background removal slider, and preview.
func (p *Plugin) renderPhotoStudio(c *ui.Context, st *StudioState) {
	ui.Column(c).FillWidth().Gap(12).Children(func() {
		// Upload Button
		ui.Row(c).FillWidth().Justify(ui.SpaceBetween).AlignItems(ui.Center).Children(func() {
			components.SecondaryButton(c, p.t("pdfsign.btn_choose_image"), func() {
				go func() {
					paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
						Filters: []mygo.FileFilter{
							{Name: "Images", Extensions: []string{"png", "jpg", "jpeg", "webp"}},
						},
					})
					if err == nil && len(paths) > 0 {
						path := paths[0]
						f, err := os.Open(path)
						if err == nil {
							img, _, err := image.Decode(f)
							f.Close()
							if err == nil {
								st.PhotoPath = path
								st.PhotoSource = img
								st.PhotoError = ""
								p.requestRedraw()
								return
							}
						}
						st.PhotoError = "Failed to load selected image"
						p.requestRedraw()
					}
				}()
			}).Height(36)

			if st.PhotoPath != "" {
				ui.Text(c, filepath.Base(st.PhotoPath)).FontSize(11).TextColor(theme.InkMuted)
			}
		})

		if st.PhotoError != "" {
			ui.Text(c, st.PhotoError).FontSize(12).TextColor(theme.Danger)
		}

		// Sliders: Threshold and Ink Mode
		if st.PhotoSource != nil {
			ui.Row(c).FillWidth().Gap(16).AlignItems(ui.Center).Children(func() {
				ui.Text(c, p.t("pdfsign.bg_threshold")).FontSize(12)
				ui.Slider(c, &st.Threshold, 0.2, 0.98).Width(200)
				ui.Text(c, fmt.Sprintf("%.0f%%", st.Threshold*100)).FontSize(12).TextColor(theme.InkMuted)
				st.processPhoto()
			})

			// Ink recoloring options
			ui.Row(c).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, p.t("pdfsign.ink_mode")).FontSize(12)

				origBtn := components.SecondaryButton(c, p.t("pdfsign.ink_orig"), func() {
					st.InkMode = InkOriginal
					st.processPhoto()
					c.Invalidate()
				}).Height(28)
				if st.InkMode == InkOriginal {
					origBtn.Background(theme.Accent).TextColor(theme.OnField).Border(2, theme.Ink).FocusRingColor(theme.Paper)
				}

				blackBtn := components.SecondaryButton(c, p.t("pdfsign.ink_black"), func() {
					st.InkMode = InkBlack
					st.processPhoto()
					c.Invalidate()
				}).Height(28)
				if st.InkMode == InkBlack {
					blackBtn.Background(theme.Accent).TextColor(theme.OnField).Border(2, theme.Ink).FocusRingColor(theme.Paper)
				}

				blueBtn := components.SecondaryButton(c, p.t("pdfsign.ink_blue"), func() {
					st.InkMode = InkBlue
					st.processPhoto()
					c.Invalidate()
				}).Height(28)
				if st.InkMode == InkBlue {
					blueBtn.Background(theme.Accent).TextColor(theme.OnField).Border(2, theme.Ink).FocusRingColor(theme.Paper)
				}
			})
		}

		// Preview Box
		previewBox := ui.Box(c.Key("photo-preview-box")).
			Width(float32(st.CanvasW)).Height(float32(st.CanvasH)).
			Background(theme.Ground).Border(2, theme.BorderQuiet).Radius(0).
			AlignItems(ui.Center).Justify(ui.Center)

		if st.PhotoBitmap != nil {
			previewBox.Children(func() {
				ui.Image(c, st.PhotoBitmap).MaxHeight(float32(st.CanvasH - 20)).MaxWidth(float32(st.CanvasW - 20))
			})
		} else {
			previewBox.Children(func() {
				ui.Text(c, p.t("pdfsign.no_photo_loaded")).FontSize(14).TextColor(theme.InkMuted)
			})
		}
	})
}
