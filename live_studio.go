package main

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"time"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
	"github.com/seiortech/mtools-sdk/pluginapi"
)

func (s *pdfSignServer) openStudioLocked(mode int, sess *pluginapi.UISession) {
	s.studio.open = true
	s.studio.mode = mode
	if s.studio.drawPreviewRef == "" {
		s.refreshDrawPreviewLocked(sess)
	}
}

func (s *pdfSignServer) handleStudioClickLocked(nodeID string, sess *pluginapi.UISession) bool {
	if !s.studio.open {
		return false
	}
	switch nodeID {
	case "studio_cancel":
		s.studio.open = false
		return true
	case "studio_swatch_black":
		s.studio.inkIdx = 0
		s.studio.eraser = false
		return true
	case "studio_swatch_blue":
		s.studio.inkIdx = 1
		s.studio.eraser = false
		return true
	case "studio_swatch_red":
		s.studio.inkIdx = 2
		s.studio.eraser = false
		return true
	case "studio_eraser":
		s.studio.eraser = !s.studio.eraser
		return true
	case "studio_undo":
		if len(s.studio.strokes) > 0 {
			s.studio.strokes = s.studio.strokes[:len(s.studio.strokes)-1]
			s.refreshDrawPreviewLocked(sess)
			return true
		}
		return false
	case "studio_clear":
		if len(s.studio.strokes) > 0 {
			s.studio.strokes = nil
			s.refreshDrawPreviewLocked(sess)
			return true
		}
		return false
	case "studio_photo_ink_orig":
		s.studio.photoInk = pdfsign.InkOriginal
		s.recomputePhotoPreviewLocked(sess)
		return true
	case "studio_photo_ink_black":
		s.studio.photoInk = pdfsign.InkBlack
		s.recomputePhotoPreviewLocked(sess)
		return true
	case "studio_photo_ink_blue":
		s.studio.photoInk = pdfsign.InkBlue
		s.recomputePhotoPreviewLocked(sess)
		return true
	case "studio_photo_crisp":
		s.studio.photoCrisp = !s.studio.photoCrisp
		s.recomputePhotoPreviewLocked(sess)
		return true
	case "studio_save_vault":
		return s.commitStudioLocked(true, false, sess)
	case "studio_place_sig":
		return s.commitStudioLocked(false, true, sess)
	}
	return false
}

func (s *pdfSignServer) refreshDrawPreviewLocked(sess *pluginapi.UISession) {
	if sess == nil {
		return
	}
	img := pdfsign.RenderStrokesToImage(studioCanvasW, studioCanvasH, s.studio.strokes)
	s.studio.drawSeq++
	fileName := fmt.Sprintf("studio-draw-%d.png", s.studio.drawSeq)
	if err := writeAssetPNG(sess.AssetsDir(), fileName, img); err != nil {
		return
	}
	if s.studio.drawPreviewFile != "" {
		_ = os.Remove(filepath.Join(sess.AssetsDir(), s.studio.drawPreviewFile))
	}
	s.studio.drawPreviewFile = fileName
	s.studio.drawPreviewRef = "asset:" + fileName
}

func (s *pdfSignServer) loadStudioPhotoLocked(photoPath string, sess *pluginapi.UISession) {
	s.errMsg = ""
	f, err := os.Open(photoPath)
	if err != nil {
		s.errMsg = fmt.Sprintf("Failed to open image: %v", err)
		return
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		s.errMsg = fmt.Sprintf("Unsupported image file: %v", err)
		return
	}
	s.studio.photoPath = photoPath
	s.studio.rawPhoto = img
	s.studio.photoThumb = pdfsign.DownscaleToMaxDim(img, 1024)
	s.studio.mode = studioModePhoto
	s.recomputePhotoPreviewLocked(sess)
}

func (s *pdfSignServer) schedulePhotoPreviewLocked(sess *pluginapi.UISession) {
	if s.studio.rawPhoto == nil {
		return
	}
	if s.studio.photoDebounce != nil {
		s.studio.photoDebounce.Stop()
	}
	s.studio.photoGen++
	gen := s.studio.photoGen
	s.studio.photoDebounce = time.AfterFunc(40*time.Millisecond, func() {
		s.mu.Lock()
		if s.studio.photoGen != gen || s.studio.rawPhoto == nil {
			s.mu.Unlock()
			return
		}
		s.recomputePhotoPreviewLocked(sess)
		tree := s.buildTreeLocked()
		s.mu.Unlock()

		if sess != nil {
			_ = sess.Render(tree)
		}
	})
}

func (s *pdfSignServer) recomputePhotoPreviewLocked(sess *pluginapi.UISession) {
	if s.studio.rawPhoto == nil {
		return
	}
	src := s.studio.photoThumb
	if src == nil {
		src = s.studio.rawPhoto
	}
	opts := pdfsign.PhotoProcessOptions{
		Threshold:  s.studio.threshold,
		Contrast:   s.studio.contrast,
		InkMode:    s.studio.photoInk,
		Crop:       pdfsign.DefaultNormalizedRect(),
		EnhanceInk: s.studio.photoCrisp,
	}
	res := pdfsign.AutoTrimAlphaBounds(pdfsign.RemoveBackground(src, opts), 8)
	s.studio.photoResult = res
	if sess == nil {
		return
	}
	s.studio.photoSeq++
	fileName := fmt.Sprintf("studio-photo-%d.png", s.studio.photoSeq)
	if err := writeAssetPNG(sess.AssetsDir(), fileName, res); err != nil {
		return
	}
	if s.studio.photoPreviewFile != "" {
		_ = os.Remove(filepath.Join(sess.AssetsDir(), s.studio.photoPreviewFile))
	}
	s.studio.photoPreviewFile = fileName
	s.studio.photoPreviewRef = "asset:" + fileName
}

func (s *pdfSignServer) studioHasResultLocked() bool {
	if s.studio.mode == studioModePhoto {
		return s.studio.rawPhoto != nil
	}
	return len(s.studio.strokes) > 0
}

func (s *pdfSignServer) buildFinalStudioImageLocked() (*image.NRGBA, string) {
	if s.studio.mode == studioModePhoto {
		if s.studio.rawPhoto == nil {
			return nil, ""
		}
		opts := pdfsign.PhotoProcessOptions{
			Threshold:  s.studio.threshold,
			Contrast:   s.studio.contrast,
			InkMode:    s.studio.photoInk,
			Crop:       pdfsign.DefaultNormalizedRect(),
			EnhanceInk: s.studio.photoCrisp,
		}
		trimmed := pdfsign.AutoTrimAlphaBounds(pdfsign.RemoveBackground(s.studio.rawPhoto, opts), 8)
		return trimmed, "photo"
	}
	if len(s.studio.strokes) == 0 {
		return nil, ""
	}
	raw := pdfsign.RenderStrokesToImage(studioCanvasW, studioCanvasH, s.studio.strokes)
	trimmed := pdfsign.AutoTrimAlphaBounds(raw, 8)
	return trimmed, "drawn"
}

func (s *pdfSignServer) commitStudioLocked(saveToVault, placeOnPage bool, sess *pluginapi.UISession) bool {
	img, kind := s.buildFinalStudioImageLocked()
	if img == nil {
		return false
	}

	var sourceID string
	if saveToVault && s.vault != nil {
		pngBytes, err := pdfsign.EncodePNGToBytes(img)
		if err != nil {
			s.errMsg = fmt.Sprintf("Failed to encode signature: %v", err)
			return true
		}
		b := img.Bounds()
		item, err := s.vault.Save("", kind, b.Dx(), b.Dy(), pngBytes)
		if err != nil {
			s.errMsg = s.T("pdfsign.vault_limit_reached")
			return true
		}
		sourceID = item.ID
		s.syncVaultAssetsLocked(sess)
	}

	if placeOnPage && s.pdfPath != "" {
		dims := s.pageDimsAtLocked(s.currentPage)
		sigW, sigH := defaultPlacementSize(img)
		x := (dims.W - sigW) / 2
		y := dims.H*0.55 - sigH/2
		s.placeSignatureAtLocked(img, sourceID, s.currentPage, x, y, sess)
	}

	// Reset studio draft after commit.
	s.studio.open = false
	if s.studio.mode == studioModeDraw {
		s.studio.strokes = nil
		s.refreshDrawPreviewLocked(sess)
	}
	return true
}

func (s *pdfSignServer) buildStudioDialogLocked() pluginapi.UINode {
	activeTab := "draw"
	if s.studio.mode == studioModePhoto {
		activeTab = "photo"
	}
	hasResult := s.studioHasResultLocked()
	vaultFull := s.vault != nil && s.vault.Count() >= pdfsign.MaxSavedSignatures

	var modeBody pluginapi.UINode
	if s.studio.mode == studioModePhoto {
		modeBody = s.buildStudioPhotoBodyLocked()
	} else {
		modeBody = s.buildStudioDrawBodyLocked()
	}

	return pluginapi.UINode{
		ID:   "studio_dlg",
		Kind: pluginapi.UINodeDialog,
		Props: mustProps(pluginapi.UIDialogProps{
			Title:       s.T("pdfsign.studio_title"),
			Subtitle:    s.T("pdfsign.studio_subtitle"),
			Open:        true,
			Width:       640,
			MaxWidth:    680,
			BorderWidth: 3,
			Shadow:      [2]int{6, 6},
			Padding:     20,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "studio_tabs",
				Kind: pluginapi.UINodeTabs,
				Props: mustProps(pluginapi.UITabsProps{
					Active: activeTab,
					Tabs: []pluginapi.SurfaceOption{
						{Value: "draw", Label: s.T("pdfsign.tab_draw")},
						{Value: "photo", Label: s.T("pdfsign.tab_photo")},
					},
				}),
			},
			modeBody,
			{
				ID:   "studio_footer",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Justify: "between",
					Align:   "center",
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "studio_cancel",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:   s.T("pdfsign.btn_cancel"),
							Variant: "secondary",
						}),
					},
					{
						ID:   "studio_footer_right",
						Kind: pluginapi.UINodeRow,
						Props: mustProps(pluginapi.UIContainerProps{
							Gap:         8,
							Align:       "center",
							NoFillWidth: true,
						}),
						Children: []pluginapi.UINode{
							{
								ID:   "studio_save_vault",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Label:    s.T("pdfsign.btn_save_vault"),
									Variant:  "secondary",
									Disabled: !hasResult || vaultFull,
								}),
							},
							{
								ID:   "studio_place_sig",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Label:    s.T("pdfsign.btn_place"),
									Variant:  "primary",
									Disabled: !hasResult,
								}),
							},
						},
					},
				},
			},
		},
	}
}

func (s *pdfSignServer) buildStudioDrawBodyLocked() pluginapi.UINode {
	hasStrokes := len(s.studio.strokes) > 0
	return pluginapi.UINode{
		ID:   "studio_draw_body",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Gap: 10,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "studio_draw_tools",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Justify: "between",
					Align:   "center",
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "studio_draw_swatches",
						Kind: pluginapi.UINodeRow,
						Props: mustProps(pluginapi.UIContainerProps{
							Gap:         8,
							Align:       "center",
							NoFillWidth: true,
						}),
						Children: []pluginapi.UINode{
							{
								ID:   "studio_swatch_black",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Variant:     "swatch",
									SwatchToken: "ink",
									Width:       24,
									Height:      24,
									Selected:    !s.studio.eraser && s.studio.inkIdx == 0,
									BorderSize:  2,
									Tooltip:     s.T("pdfsign.ink_black"),
								}),
							},
							{
								ID:   "studio_swatch_blue",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Variant:     "swatch",
									SwatchToken: "blue_ink",
									Width:       24,
									Height:      24,
									Selected:    !s.studio.eraser && s.studio.inkIdx == 1,
									BorderSize:  2,
									Tooltip:     s.T("pdfsign.ink_blue"),
								}),
							},
							{
								ID:   "studio_swatch_red",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Variant:     "swatch",
									SwatchToken: "danger",
									Width:       24,
									Height:      24,
									Selected:    !s.studio.eraser && s.studio.inkIdx == 2,
									BorderSize:  2,
									Tooltip:     s.T("pdfsign.ink_red"),
								}),
							},
							{
								ID:   "studio_eraser",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Label:    s.T("pdfsign.tool_eraser"),
									Icon:     "eraser",
									Variant:  "secondary",
									Selected: s.studio.eraser,
									Height:   28,
								}),
							},
						},
					},
					{
						ID:   "studio_draw_acts",
						Kind: pluginapi.UINodeRow,
						Props: mustProps(pluginapi.UIContainerProps{
							Gap:         6,
							Align:       "center",
							NoFillWidth: true,
						}),
						Children: []pluginapi.UINode{
							{
								ID:   "studio_undo",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Label:    s.T("pdfsign.tool_undo"),
									Variant:  "secondary",
									Disabled: !hasStrokes,
									Height:   28,
								}),
							},
							{
								ID:   "studio_clear",
								Kind: pluginapi.UINodeButton,
								Props: mustProps(pluginapi.UIButtonProps{
									Label:    s.T("pdfsign.tool_clear"),
									Variant:  "secondary",
									Disabled: !hasStrokes,
									Height:   28,
								}),
							},
						},
					},
				},
			},
			{
				ID:   "studio_draw_canvas",
				Kind: pluginapi.UINodeCanvas,
				Props: mustProps(pluginapi.UICanvasProps{
					Width:       studioCanvasW,
					Height:      studioCanvasH,
					Src:         s.studio.drawPreviewRef,
					Surface:     "paper",
					BorderWidth: 2,
					Interactive: true,
					BrushToken:  studioInkToken(s.studio.inkIdx),
					BrushWidth:  s.studio.penWidth,
					Eraser:      s.studio.eraser,
					Ops: []pluginapi.UICanvasOp{
						{
							Kind:  "line",
							Token: "muted",
							X:     32,
							Y:     185,
							W:     studioCanvasW - 64,
							Width: 1,
						},
					},
				}),
			},
		},
	}
}

func (s *pdfSignServer) buildStudioPhotoBodyLocked() pluginapi.UINode {
	var previewChildren []pluginapi.UINode
	if s.studio.photoPreviewRef != "" {
		previewChildren = []pluginapi.UINode{
			{
				ID:   "studio_photo_img",
				Kind: pluginapi.UINodeImage,
				Props: mustProps(pluginapi.UIImageProps{
					Src:       s.studio.photoPreviewRef,
					Fit:       "contain",
					Width:     studioCanvasW - 24,
					Height:    studioCanvasH - 24,
					MaxWidth:  studioCanvasW - 24,
					MaxHeight: studioCanvasH - 24,
				}),
			},
		}
	} else {
		previewChildren = []pluginapi.UINode{
			{
				ID:   "studio_photo_empty",
				Kind: pluginapi.UINodeText,
				Props: mustProps(pluginapi.UITextProps{
					Text:    s.T("pdfsign.no_photo_loaded"),
					Variant: "muted",
					Align:   "center",
				}),
			},
		}
	}

	return pluginapi.UINode{
		ID:   "studio_photo_body",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Gap: 10,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "studio_photo_controls_1",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Justify: "between",
					Align:   "center",
					Gap:     12,
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "studio_choose_photo",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:   s.T("pdfsign.btn_choose_image"),
							Variant: "secondary",
							Icon:    "image",
							PickFile: &pluginapi.UIFilePickerProps{
								Label:      s.T("pdfsign.btn_choose_image"),
								Mode:       "open",
								Extensions: []string{"png", "jpg", "jpeg", "webp"},
							},
						}),
					},
					{
						ID:   "studio_threshold",
						Kind: pluginapi.UINodeSlider,
						Props: mustProps(pluginapi.UISliderProps{
							Label:       s.T("pdfsign.bg_threshold"),
							Min:         0.20,
							Max:         0.98,
							Step:        0.02,
							Value:       s.studio.threshold,
							Width:       280,
							ValueScale:  100,
							ValueSuffix: "%",
							Disabled:    s.studio.rawPhoto == nil,
						}),
					},
				},
			},
			{
				ID:   "studio_photo_controls_2",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Gap:   8,
					Align: "center",
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "studio_ink_lbl",
						Kind: pluginapi.UINodeText,
						Props: mustProps(pluginapi.UITextProps{
							Text:     s.T("pdfsign.ink_mode"),
							FontSize: 12,
							Variant:  "muted",
						}),
					},
					{
						ID:   "studio_photo_ink_orig",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.ink_orig"),
							Variant:  "secondary",
							Selected: s.studio.photoInk == pdfsign.InkOriginal,
							Disabled: s.studio.rawPhoto == nil,
							Height:   28,
						}),
					},
					{
						ID:   "studio_photo_ink_black",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.ink_black"),
							Variant:  "secondary",
							Selected: s.studio.photoInk == pdfsign.InkBlack,
							Disabled: s.studio.rawPhoto == nil,
							Height:   28,
						}),
					},
					{
						ID:   "studio_photo_ink_blue",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.ink_blue"),
							Variant:  "secondary",
							Selected: s.studio.photoInk == pdfsign.InkBlue,
							Disabled: s.studio.rawPhoto == nil,
							Height:   28,
						}),
					},
					{
						ID:   "studio_photo_crisp",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.crisp_ink"),
							Variant:  "secondary",
							Selected: s.studio.photoCrisp,
							Disabled: s.studio.rawPhoto == nil,
							Height:   28,
						}),
					},
				},
			},
			{
				ID:   "studio_photo_preview_box",
				Kind: pluginapi.UINodeColumn,
				Props: mustProps(pluginapi.UIContainerProps{
					Width:      studioCanvasW,
					Height:     studioCanvasH,
					Surface:    "paper",
					BorderSize: 2,
					Justify:    "center",
					Align:      "center",
				}),
				Children: previewChildren,
			},
		},
	}
}
