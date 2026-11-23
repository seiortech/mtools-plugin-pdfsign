package main

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
	"github.com/seiortech/mtools-sdk/pluginapi"
)

func (s *pdfSignServer) buildTreeLocked() pluginapi.UITree {
	s.rev++
	if s.pdfPath == "" {
		return pluginapi.UITree{
			Rev:  s.rev,
			Root: s.buildEmptyTreeLocked(),
		}
	}
	return pluginapi.UITree{
		Rev:  s.rev,
		Root: s.buildWorkbenchTreeLocked(),
	}
}

func (s *pdfSignServer) buildEmptyTreeLocked() pluginapi.UINode {
	children := []pluginapi.UINode{
		{
			ID:   "empty_card",
			Kind: pluginapi.UINodeEmpty,
			Props: mustProps(pluginapi.UIEmptyProps{
				Title:       s.T("pdfsign.empty_title"),
				Subtitle:    s.T("pdfsign.empty_desc"),
				ActionLabel: s.T("pdfsign.btn_select_file"),
				Icon:        "file-text",
				IconSize:    36,
				Width:       440,
				BorderWidth: 3,
				Shadow:      [2]int{5, 5},
				PickFile: &pluginapi.UIFilePickerProps{
					Label:      s.T("pdfsign.btn_select_file"),
					Mode:       "open",
					Extensions: []string{"pdf"},
				},
			}),
		},
	}
	if s.errMsg != "" {
		children = append(children, pluginapi.UINode{
			ID:   "empty_err",
			Kind: pluginapi.UINodeText,
			Props: mustProps(pluginapi.UITextProps{
				Text:    s.errMsg,
				Variant: "danger",
			}),
		})
	}
	// Work around empty picker using a dedicated UIEventChange target ID matches empty_open_pdf / empty_card.
	children[0].ID = "empty_open_pdf"
	return pluginapi.UINode{
		ID:   "empty_root",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Fill:    true,
			Surface: "ground",
			Justify: "center",
			Align:   "center",
			Gap:     12,
		}),
		Children: children,
	}
}

func (s *pdfSignServer) buildWorkbenchTreeLocked() pluginapi.UINode {
	splitW := s.railWidth
	if s.railCollapsed {
		splitW = 48
	}
	if splitW <= 0 {
		splitW = defaultRailWidth
	}

	rootChildren := []pluginapi.UINode{
		s.buildTopBarNodeLocked(),
		{
			ID:   "main_split",
			Kind: pluginapi.UINodeSplit,
			Props: mustProps(pluginapi.UISplitProps{
				Axis:               "h",
				Size:               splitW,
				Min:                220,
				Max:                420,
				DefaultSize:        defaultRailWidth,
				Collapsible:        true,
				Collapsed:          s.railCollapsed,
				ResetOnDoubleClick: true,
			}),
			Children: []pluginapi.UINode{
				s.buildViewportStackLocked(),
				s.buildRightPanelLocked(),
			},
		},
	}
	if s.studio.open {
		rootChildren = append(rootChildren, s.buildStudioDialogLocked())
	}

	return pluginapi.UINode{
		ID:   "workbench_root",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Fill:      true,
			Surface:   "ground",
			Shortcuts: []string{"Escape", "Delete", "Backspace", "L", "Cmd+L", "Ctrl+L", "Ctrl+D", "Cmd+D"},
		}),
		Children: rootChildren,
	}
}

func (s *pdfSignServer) buildTopBarNodeLocked() pluginapi.UINode {
	defaultOut := strings.TrimSuffix(s.pdfPath, filepath.Ext(s.pdfPath)) + "-signed.pdf"
	addVariant := "secondary"
	if s.vault != nil && s.vault.Count() == 0 {
		addVariant = "primary"
	}
	toggleIcon := "collapse-right"
	toggleTip := "Collapse Panel"
	if s.railCollapsed {
		toggleIcon = "collapse-left"
		toggleTip = "Expand Panel"
	}

	leftGroup := pluginapi.UINode{
		ID:   "topbar_left",
		Kind: pluginapi.UINodeRow,
		Props: mustProps(pluginapi.UIContainerProps{
			Gap:         10,
			Align:       "center",
			NoFillWidth: true,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "open_pdf",
				Kind: pluginapi.UINodeButton,
				Props: mustProps(pluginapi.UIButtonProps{
					Label:    s.T("pdfsign.btn_open_pdf"),
					Variant:  "secondary",
					Icon:     "file-text",
					Disabled: s.exporting,
					PickFile: &pluginapi.UIFilePickerProps{
						Label:      s.T("pdfsign.btn_open_pdf"),
						Mode:       "open",
						Extensions: []string{"pdf"},
					},
				}),
			},
			{
				ID:   "doc_name",
				Kind: pluginapi.UINodeText,
				Props: mustProps(pluginapi.UITextProps{
					Text:       filepath.Base(s.pdfPath),
					FontSize:   13,
					FontWeight: 600,
					SingleLine: true,
					Tooltip:    s.pdfPath,
				}),
			},
		},
	}

	centerGroup := pluginapi.UINode{
		ID:   "topbar_center",
		Kind: pluginapi.UINodeRow,
		Props: mustProps(pluginapi.UIContainerProps{
			Gap:         8,
			Align:       "center",
			NoFillWidth: true,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "page_stepper",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Surface:     "ground",
					BorderSize:  1,
					PaddingH:    4,
					PaddingV:    2,
					Gap:         4,
					Align:       "center",
					NoFillWidth: true,
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "page_prev",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Icon:     "chevron-left",
							IconOnly: true,
							Variant:  "ghost",
							Height:   28,
							Width:    28,
							Disabled: s.currentPage <= 1,
						}),
					},
					{
						ID:   "page_readout",
						Kind: pluginapi.UINodeText,
						Props: mustProps(pluginapi.UITextProps{
							Text:        fmt.Sprintf("%d / %d", s.currentPage, max(s.pageCount, 1)),
							FontSize:    12,
							FontWeight:  600,
							SingleLine:  true,
							TabularNums: true,
							PaddingH:    6,
						}),
					},
					{
						ID:   "page_next",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Icon:     "chevron-right",
							IconOnly: true,
							Variant:  "ghost",
							Height:   28,
							Width:    28,
							Disabled: s.currentPage >= s.pageCount,
						}),
					},
				},
			},
			{
				ID:   "zoom_box",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Surface:     "ground",
					BorderSize:  1,
					PaddingH:    4,
					PaddingV:    2,
					Gap:         4,
					Align:       "center",
					NoFillWidth: true,
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "zoom_out",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Icon:     "zoom-out",
							IconOnly: true,
							Variant:  "ghost",
							Height:   28,
							Width:    28,
							Disabled: s.zoom <= minZoomFactor+0.01,
						}),
					},
					{
						ID:   "zoom_readout",
						Kind: pluginapi.UINodeText,
						Props: mustProps(pluginapi.UITextProps{
							Text:        fmt.Sprintf("%.0f%%", s.effectiveZoomLocked()*100),
							FontSize:    12,
							FontWeight:  600,
							SingleLine:  true,
							TabularNums: true,
							PaddingH:    4,
						}),
					},
					{
						ID:   "zoom_in",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Icon:     "zoom-in",
							IconOnly: true,
							Variant:  "ghost",
							Height:   28,
							Width:    28,
							Disabled: s.zoom >= maxZoomFactor-0.01,
						}),
					},
					{
						ID:   "zoom_fit",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:   s.T("pdfsign.zoom_fit"),
							Variant: "secondary",
							Height:  28,
						}),
					},
				},
			},
		},
	}

	rightChildren := []pluginapi.UINode{}
	if s.errMsg != "" {
		rightChildren = append(rightChildren, pluginapi.UINode{
			ID:   "topbar_err",
			Kind: pluginapi.UINodeText,
			Props: mustProps(pluginapi.UITextProps{
				Text:       s.errMsg,
				Variant:    "danger",
				FontSize:   12,
				SingleLine: true,
			}),
		})
	}
	rightChildren = append(rightChildren,
		pluginapi.UINode{
			ID:   "add_sig_top",
			Kind: pluginapi.UINodeButton,
			Props: mustProps(pluginapi.UIButtonProps{
				Label:    s.T("pdfsign.btn_add_signature"),
				Variant:  addVariant,
				Icon:     "signature",
				Disabled: s.exporting,
			}),
		},
		pluginapi.UINode{
			ID:   "finish_sign",
			Kind: pluginapi.UINodeButton,
			Props: mustProps(pluginapi.UIButtonProps{
				Label:    s.T("pdfsign.btn_finish_export"),
				Variant:  "primary",
				Disabled: len(s.placements) == 0 || s.exporting,
				PickFile: &pluginapi.UIFilePickerProps{
					Label:      s.T("pdfsign.btn_finish_export"),
					Mode:       "save",
					Value:      defaultOut,
					Extensions: []string{"pdf"},
				},
			}),
		},
		pluginapi.UINode{
			ID:   "rail_toggle",
			Kind: pluginapi.UINodeButton,
			Props: mustProps(pluginapi.UIButtonProps{
				Icon:     toggleIcon,
				IconOnly: true,
				Variant:  "secondary",
				Width:    32,
				Height:   32,
				Tooltip:  toggleTip,
			}),
		},
	)

	rightGroup := pluginapi.UINode{
		ID:   "topbar_right",
		Kind: pluginapi.UINodeRow,
		Props: mustProps(pluginapi.UIContainerProps{
			Gap:         8,
			Align:       "center",
			NoFillWidth: true,
		}),
		Children: rightChildren,
	}

	return pluginapi.UINode{
		ID:   "topbar",
		Kind: pluginapi.UINodeRow,
		Props: mustProps(pluginapi.UIContainerProps{
			Height:      52,
			PaddingH:    16,
			Justify:     "between",
			Align:       "center",
			Surface:     "paper",
			BorderSides: [4]int{0, 0, 2, 0},
		}),
		Children: []pluginapi.UINode{leftGroup, centerGroup, rightGroup},
	}
}

func (s *pdfSignServer) buildViewportStackLocked() pluginapi.UINode {
	z := s.effectiveZoomLocked()
	pageNodes := make([]pluginapi.UINode, 0, s.pageCount)

	for p := 1; p <= s.pageCount; p++ {
		dims := s.pageDimsAtLocked(p)
		pw := int(math.Round(dims.W * z))
		ph := int(math.Round(dims.H * z))
		if pw < 100 {
			pw = 100
		}
		if ph < 100 {
			ph = 100
		}

		if absInt(p-s.currentPage) > 4 {
			pageNodes = append(pageNodes, pluginapi.UINode{
				ID:   fmt.Sprintf("page-spacer-%d", p),
				Kind: pluginapi.UINodeColumn,
				Props: mustProps(pluginapi.UIContainerProps{
					Width:       pw,
					Height:      ph,
					Surface:     "paper",
					BorderSize:  1,
					NoFillWidth: true,
					Justify:     "center",
					Align:       "center",
				}),
				Children: []pluginapi.UINode{
					{
						ID:   fmt.Sprintf("page-spacer-lbl-%d", p),
						Kind: pluginapi.UINodeText,
						Props: mustProps(pluginapi.UITextProps{
							Text:    fmt.Sprintf("Page %d", p),
							Variant: "muted",
						}),
					},
				},
			})
			continue
		}

		assetRef := s.pageAssetRefLocked(p)
		canvasChildren := make([]pluginapi.UINode, 0, 4)
		var activePlaced *pdfsign.PlacedSignature
		for i := range s.placements {
			pl := s.placements[i]
			if pl.Page != p {
				continue
			}
			px := pl.X * z
			py := pl.Y * z
			w := max(int(math.Round(pl.Width*z)), 24)
			h := max(int(math.Round(pl.Height*z)), 16)
			aspect := pl.Width / max(pl.Height, 1)
			imgRef := s.placedAssets[pl.ID]

			var dragChildren []pluginapi.UINode
			if imgRef != "" {
				dragChildren = []pluginapi.UINode{
					{
						ID:   "placed_img_" + pl.ID,
						Kind: pluginapi.UINodeImage,
						Props: mustProps(pluginapi.UIImageProps{
							Src:    imgRef,
							Fit:    "contain",
							Width:  w,
							Height: h,
						}),
					},
				}
			}

			canvasChildren = append(canvasChildren, pluginapi.UINode{
				ID:   "placed_" + pl.ID,
				Kind: pluginapi.UINodeDraggable,
				Props: mustProps(pluginapi.UIDraggableProps{
					X:             px,
					Y:             py,
					Width:         w,
					Height:        h,
					BoundW:        pw,
					BoundH:        ph,
					Selected:      pl.ID == s.activeID,
					Resizable:     pl.ID == s.activeID && !pl.Locked,
					Handles:       "all",
					KeepAspect:    s.lockAspect,
					AspectRatio:   aspect,
					ShowLockBadge: pl.Locked || (pl.ID == s.activeID && s.lockAspect),
					Disabled:      pl.Locked,
				}),
				Children: dragChildren,
			})
			if pl.ID == s.activeID {
				copyPl := pl
				activePlaced = &copyPl
			}
		}

		if activePlaced != nil {
			canvasChildren = append(canvasChildren, s.buildCanvasPlacementBarLocked(*activePlaced, z, pw, ph))
		}

		// Bottom-right page tag overlay (matches HEAD:pdfsign/view.go renderPageInScrollStack)
		rightVal := 8.0
		bottomVal := 8.0
		canvasChildren = append(canvasChildren, pluginapi.UINode{
			ID:   fmt.Sprintf("page_badge_%d", p),
			Kind: pluginapi.UINodeRow,
			Props: mustProps(pluginapi.UIContainerProps{
				Absolute:    true,
				Right:       &rightVal,
				Bottom:      &bottomVal,
				PaddingH:    6,
				PaddingV:    2,
				Surface:     "paper",
				BorderSize:  1,
				NoFillWidth: true,
			}),
			Children: []pluginapi.UINode{
				{
					ID:   fmt.Sprintf("page_badge_txt_%d", p),
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:     fmt.Sprintf("%d", p),
						FontSize: 10,
						Variant:  "muted",
					}),
				},
			},
		})

		if assetRef == "" {
			loadingText := s.T("pdfsign.rendering_page")
			loadingVariant := "muted"
			for ek, emsg := range s.previewRenderErr {
				if ek.docGen == s.docGen && ek.pageNum == p && emsg != "" {
					loadingText = s.T("pdfsign.preview_render_failed")
					loadingVariant = "danger"
					break
				}
			}
			canvasChildren = append([]pluginapi.UINode{
				{
					ID:   fmt.Sprintf("page_loading_%d", p),
					Kind: pluginapi.UINodeColumn,
					Props: mustProps(pluginapi.UIContainerProps{
						Width:       pw,
						Height:      ph,
						Justify:     "center",
						Align:       "center",
						NoFillWidth: true,
					}),
					Children: []pluginapi.UINode{
						{
							ID:   fmt.Sprintf("page_loading_txt_%d", p),
							Kind: pluginapi.UINodeText,
							Props: mustProps(pluginapi.UITextProps{
								Text:    loadingText,
								Variant: loadingVariant,
							}),
						},
					},
				},
			}, canvasChildren...)
		}

		pageNodes = append(pageNodes, pluginapi.UINode{
			ID:   fmt.Sprintf("page_%d", p),
			Kind: pluginapi.UINodeCanvas,
			Props: mustProps(pluginapi.UICanvasProps{
				Width:         pw,
				Height:        ph,
				Src:           assetRef,
				Surface:       "paper",
				BorderWidth:   1,
				Shadow:        [2]int{6, 6},
				DropTarget:    true,
				DropHighlight: true,
				Clickable:     true,
			}),
			Children: canvasChildren,
		})
	}

	viewportNode := pluginapi.UINode{
		ID:   "viewport",
		Kind: pluginapi.UINodeScroll,
		Props: mustProps(pluginapi.UIContainerProps{
			Fill:        true,
			Grow:        true,
			Padding:     24,
			Gap:         24,
			Align:       "center",
			Justify:     "center",
			Surface:     "ground",
			ScrollBoth:  true,
			TrackScroll: true,
			Clickable:   true,
			ScrollTarget: &pluginapi.UIScrollTarget{
				Y:      s.targetScrollY,
				Seq:    s.scrollSeq,
				Smooth: s.scrollSeq > 1,
			},
		}),
		Children: pageNodes,
	}

	var stackChildren []pluginapi.UINode
	if s.previewEngineErr != nil {
		titleKey := "pdfsign.pdfium_missing_title"
		if errors.Is(s.previewEngineErr, pdfsign.ErrPDFiumInvalid) {
			titleKey = "pdfsign.pdfium_invalid_title"
		}
		banner := pluginapi.UINode{
			ID:   "pdfium_engine_banner",
			Kind: pluginapi.UINodeRow,
			Props: mustProps(pluginapi.UIContainerProps{
				Surface:     "paper",
				BorderSize:  1,
				BorderToken: "danger",
				Padding:     12,
				Gap:         12,
				Align:       "center",
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "pdfium_engine_msg_col",
					Kind: pluginapi.UINodeColumn,
					Props: mustProps(pluginapi.UIContainerProps{
						Grow: true,
						Gap:  2,
					}),
					Children: []pluginapi.UINode{
						{
							ID:   "pdfium_engine_title",
							Kind: pluginapi.UINodeText,
							Props: mustProps(pluginapi.UITextProps{
								Text:    s.T(titleKey),
								Variant: "danger",
							}),
						},
						{
							ID:   "pdfium_engine_desc",
							Kind: pluginapi.UINodeText,
							Props: mustProps(pluginapi.UITextProps{
								Text:    s.T("pdfsign.pdfium_missing_desc"),
								Variant: "muted",
							}),
						},
					},
				},
				{
					ID:   "pdfium_engine_actions",
					Kind: pluginapi.UINodeRow,
					Props: mustProps(pluginapi.UIContainerProps{
						Gap:         8,
						Align:       "center",
						NoFillWidth: true,
					}),
					Children: []pluginapi.UINode{
						{
							ID:   "download_pdfium",
							Kind: pluginapi.UINodeButton,
							Props: mustProps(pluginapi.UIButtonProps{
								Label: func() string {
									if s.pdfiumDownloading && s.pdfiumDownloadMsg != "" {
										return s.pdfiumDownloadMsg
									}
									if s.pdfiumDownloadErr != "" {
										return "Retry Download"
									}
									return s.T("pdfsign.btn_download_pdfium")
								}(),
								Variant:  "primary",
								Icon:     "download",
								Disabled: s.pdfiumDownloading,
								Tooltip:  s.pdfiumDownloadErr,
							}),
						},
						{
							ID:   "open_settings",
							Kind: pluginapi.UINodeButton,
							Props: mustProps(pluginapi.UIButtonProps{
								Label:      s.T("pdfsign.btn_open_settings"),
								Variant:    "secondary",
								Icon:       "settings",
								HostAction: "open_app_settings",
							}),
						},
						{
							ID:   "retry_pdfium",
							Kind: pluginapi.UINodeButton,
							Props: mustProps(pluginapi.UIButtonProps{
								Label:   s.T("pdfsign.btn_reload_preview"),
								Variant: "secondary",
								Icon:    "refresh-cw",
							}),
						},
					},
				},
			},
		}
		stackChildren = []pluginapi.UINode{
			{
				ID:   "viewport_with_banner",
				Kind: pluginapi.UINodeColumn,
				Props: mustProps(pluginapi.UIContainerProps{
					Fill: true,
					Grow: true,
				}),
				Children: []pluginapi.UINode{banner, viewportNode},
			},
		}
	} else {
		stackChildren = []pluginapi.UINode{viewportNode}
	}

	return pluginapi.UINode{
		ID:   "viewport_stack",
		Kind: pluginapi.UINodeStack,
		Props: mustProps(pluginapi.UIContainerProps{
			Fill: true,
			Grow: true,
		}),
		Children: stackChildren,
	}
}

func (s *pdfSignServer) buildCanvasPlacementBarLocked(pl pdfsign.PlacedSignature, z float64, pw, ph int) pluginapi.UINode {
	px := pl.X * z
	py := pl.Y * z
	h := float64(max(int(math.Round(pl.Height*z)), 16))
	const barW = 224.0
	const barH = 34.0
	barTop := py - barH - 4
	if barTop < 4 {
		barTop = py + h + 6
	}
	if barTop+barH > float64(ph)-4 {
		barTop = max(4.0, float64(ph)-barH-4)
	}
	maxLeft := max(4.0, float64(pw)-barW-4)
	barLeft := min(max(px, 4.0), maxLeft)

	lockIcon := "unlock"
	lockTip := s.T("pdfsign.lock_placement")
	if pl.Locked {
		lockIcon = "lock"
		lockTip = s.T("pdfsign.unlock_placement")
	}

	return pluginapi.UINode{
		ID:   "sel_bar_" + pl.ID,
		Kind: pluginapi.UINodeRow,
		Props: mustProps(pluginapi.UIContainerProps{
			Absolute:    true,
			Left:        &barLeft,
			Top:         &barTop,
			PaddingH:    4,
			PaddingV:    3,
			Gap:         3,
			Align:       "center",
			Surface:     "paper",
			BorderSize:  1,
			BorderToken: "ink",
			NoFillWidth: true,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "sel_lock",
				Kind: pluginapi.UINodeButton,
				Props: mustProps(pluginapi.UIButtonProps{
					Icon:     lockIcon,
					IconOnly: true,
					Selected: pl.Locked,
					Variant:  "secondary",
					Width:    26,
					Height:   26,
					PaddingX: 4,
					PaddingY: 4,
					Tooltip:  lockTip,
				}),
			},
			{
				ID:   "sel_rot_left",
				Kind: pluginapi.UINodeButton,
				Props: mustProps(pluginapi.UIButtonProps{
					Label:    s.T("pdfsign.rotate_left"),
					Disabled: pl.Locked,
					Variant:  "secondary",
					Height:   26,
					PaddingX: 6,
					PaddingY: 4,
				}),
			},
			{
				ID:   "sel_rot_right",
				Kind: pluginapi.UINodeButton,
				Props: mustProps(pluginapi.UIButtonProps{
					Label:    s.T("pdfsign.rotate_right"),
					Disabled: pl.Locked,
					Variant:  "secondary",
					Height:   26,
					PaddingX: 6,
					PaddingY: 4,
				}),
			},
			{
				ID:   "sel_dup",
				Kind: pluginapi.UINodeButton,
				Props: mustProps(pluginapi.UIButtonProps{
					Icon:     "copy",
					IconOnly: true,
					Variant:  "secondary",
					Width:    26,
					Height:   26,
					PaddingX: 4,
					PaddingY: 4,
					Tooltip:  s.T("pdfsign.btn_duplicate") + " (Ctrl+D)",
				}),
			},
			{
				ID:   "sel_del",
				Kind: pluginapi.UINodeButton,
				Props: mustProps(pluginapi.UIButtonProps{
					Icon:     "trash",
					IconOnly: true,
					Disabled: pl.Locked,
					Variant:  "danger",
					Width:    26,
					Height:   26,
					PaddingX: 4,
					PaddingY: 4,
					Tooltip:  s.T("pdfsign.delete") + " (Del)",
				}),
			},
		},
	}
}

func (s *pdfSignServer) buildRightPanelLocked() pluginapi.UINode {
	if s.railCollapsed {
		return pluginapi.UINode{
			ID:   "right_rail_collapsed",
			Kind: pluginapi.UINodeColumn,
			Props: mustProps(pluginapi.UIContainerProps{
				Width:       48,
				FillHeight:  true,
				PaddingV:    12,
				Gap:         12,
				Align:       "center",
				Surface:     "paper",
				BorderSides: [4]int{0, 0, 0, 2},
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "rail_expand",
					Kind: pluginapi.UINodeButton,
					Props: mustProps(pluginapi.UIButtonProps{
						Icon:     "collapse-left",
						IconOnly: true,
						Variant:  "ghost",
						Width:    32,
						Height:   32,
						Tooltip:  "Expand Panel",
					}),
				},
				{
					ID:   "rail_sig_ic",
					Kind: pluginapi.UINodeIcon,
					Props: mustProps(pluginapi.UIIconProps{
						Name: "signature",
						Size: 18,
						Tone: "accent",
					}),
				},
				{
					ID:   "rail_pg_badge",
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:        fmt.Sprintf("%d/%d", s.currentPage, max(s.pageCount, 1)),
						FontSize:    11,
						FontWeight:  600,
						TabularNums: true,
					}),
				},
			},
		}
	}

	var sections []pluginapi.UINode
	if banner := s.buildExportBannerLocked(); banner != nil {
		sections = append(sections, *banner)
	}
	sections = append(sections, s.buildVaultSectionLocked())
	if insp := s.buildActiveInspectorLocked(); insp != nil {
		sections = append(sections, *insp)
	} else {
		sections = append(sections, s.buildPageJumpSectionLocked())
	}

	return pluginapi.UINode{
		ID:   "right_panel",
		Kind: pluginapi.UINodeScroll,
		Props: mustProps(pluginapi.UIContainerProps{
			FillHeight:  true,
			Padding:     14,
			Gap:         14,
			Surface:     "paper",
			BorderSides: [4]int{0, 0, 0, 2},
		}),
		Children: sections,
	}
}

func (s *pdfSignServer) buildExportBannerLocked() *pluginapi.UINode {
	if s.exporting {
		node := pluginapi.UINode{
			ID:   "export_banner_busy",
			Kind: pluginapi.UINodeColumn,
			Props: mustProps(pluginapi.UIContainerProps{
				Padding:    12,
				Gap:        8,
				Surface:    "ground",
				BorderSize: 2,
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "export_prog",
					Kind: pluginapi.UINodeProgress,
					Props: mustProps(pluginapi.UIProgressProps{
						Done:  50,
						Total: 100,
						Label: "Signing PDF...",
					}),
				},
			},
		}
		return &node
	}
	if s.exportErr != "" {
		node := pluginapi.UINode{
			ID:   "export_banner_err",
			Kind: pluginapi.UINodeColumn,
			Props: mustProps(pluginapi.UIContainerProps{
				Padding:     12,
				Gap:         8,
				Surface:     "paper",
				BorderSize:  2,
				BorderToken: "danger",
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "export_err_txt",
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:    s.exportErr,
						Variant: "danger",
					}),
				},
				{
					ID:   "dismiss_export",
					Kind: pluginapi.UINodeButton,
					Props: mustProps(pluginapi.UIButtonProps{
						Label:   "Dismiss",
						Variant: "secondary",
						Height:  28,
					}),
				},
			},
		}
		return &node
	}
	if s.exportDone && s.exportPath != "" {
		node := pluginapi.UINode{
			ID:   "export_banner_ok",
			Kind: pluginapi.UINodeColumn,
			Props: mustProps(pluginapi.UIContainerProps{
				Padding:     12,
				Gap:         8,
				Surface:     "field_support",
				BorderSize:  2,
				BorderToken: "accent",
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "export_ok_hdr",
					Kind: pluginapi.UINodeRow,
					Props: mustProps(pluginapi.UIContainerProps{
						Justify: "between",
						Align:   "center",
					}),
					Children: []pluginapi.UINode{
						{
							ID:   "export_ok_title",
							Kind: pluginapi.UINodeText,
							Props: mustProps(pluginapi.UITextProps{
								Text:       s.T("pdfsign.sign_complete"),
								Variant:    "on_field",
								FontSize:   12,
								FontWeight: 700,
								Grow:       true,
							}),
						},
						{
							ID:   "dismiss_export",
							Kind: pluginapi.UINodeButton,
							Props: mustProps(pluginapi.UIButtonProps{
								Icon:     "close",
								IconOnly: true,
								Variant:  "ghost",
								Width:    24,
								Height:   24,
							}),
						},
					},
				},
				{
					ID:   "export_file_lbl",
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:       filepath.Base(s.exportPath),
						Variant:    "on_field",
						FontSize:   11,
						SingleLine: true,
					}),
				},
				{
					ID:   "export_actions",
					Kind: pluginapi.UINodeRow,
					Props: mustProps(pluginapi.UIContainerProps{
						Gap:  6,
						Wrap: true,
					}),
					Children: []pluginapi.UINode{
						{
							ID:   "reveal_pdf_btn",
							Kind: pluginapi.UINodeButton,
							Props: mustProps(pluginapi.UIButtonProps{
								Label:       s.T("pdfsign.reveal_in_folder"),
								Variant:     "secondary",
								ShellAction: "reveal",
								ShellPath:   s.exportPath,
								Height:      28,
							}),
						},
						{
							ID:   "open_signed_pdf_btn",
							Kind: pluginapi.UINodeButton,
							Props: mustProps(pluginapi.UIButtonProps{
								Label:       s.T("pdfsign.open_signed_pdf"),
								Variant:     "primary",
								ShellAction: "open",
								ShellPath:   s.exportPath,
								Height:      28,
							}),
						},
					},
				},
			},
		}
		return &node
	}
	return nil
}

func (s *pdfSignServer) buildVaultSectionLocked() pluginapi.UINode {
	var items []pdfsign.SignatureItem
	if s.vault != nil {
		items = s.vault.List()
	}

	sectionChildren := []pluginapi.UINode{
		{
			ID:   "vault_hdr",
			Kind: pluginapi.UINodeRow,
			Props: mustProps(pluginapi.UIContainerProps{
				Justify: "between",
				Align:   "center",
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "vault_title",
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:       s.T("pdfsign.vault_header"),
						FontSize:   14,
						FontWeight: 700,
					}),
				},
				{
					ID:   "vault_cnt",
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:        fmt.Sprintf("%d / %d", len(items), pdfsign.MaxSavedSignatures),
						Variant:     "muted",
						TabularNums: true,
					}),
				},
			},
		},
		{
			ID:   "vault_add_row",
			Kind: pluginapi.UINodeRow,
			Props: mustProps(pluginapi.UIContainerProps{
				Gap: 8,
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "vault_draw_btn",
					Kind: pluginapi.UINodeButton,
					Props: mustProps(pluginapi.UIButtonProps{
						Label:   s.T("pdfsign.draw_sig"),
						Variant: "secondary",
						Grow:    true,
					}),
				},
				{
					ID:   "vault_photo_btn",
					Kind: pluginapi.UINodeButton,
					Props: mustProps(pluginapi.UIButtonProps{
						Label:   s.T("pdfsign.photo_sig"),
						Variant: "secondary",
						Grow:    true,
					}),
				},
			},
		},
	}

	if len(items) == 0 {
		sectionChildren = append(sectionChildren, pluginapi.UINode{
			ID:   "vault_empty_box",
			Kind: pluginapi.UINodeColumn,
			Props: mustProps(pluginapi.UIContainerProps{
				Padding:    14,
				Surface:    "ground",
				BorderSize: 1,
				Align:      "center",
			}),
			Children: []pluginapi.UINode{
				{
					ID:   "vault_empty_txt",
					Kind: pluginapi.UINodeText,
					Props: mustProps(pluginapi.UITextProps{
						Text:    s.T("pdfsign.vault_empty"),
						Variant: "muted",
						Align:   "center",
					}),
				},
			},
		})
	} else {
		for _, it := range items {
			ref := s.vaultAssets[it.ID]
			cardChildren := []pluginapi.UINode{}
			if ref != "" {
				cardChildren = append(cardChildren, pluginapi.UINode{
					ID:   "vault_img_" + it.ID,
					Kind: pluginapi.UINodeImage,
					Props: mustProps(pluginapi.UIImageProps{
						Src:    ref,
						Fit:    "contain",
						Width:  150,
						Height: 42,
					}),
				})
			}
			cardChildren = append(cardChildren, pluginapi.UINode{
				ID:   "vault_row_" + it.ID,
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Gap:   6,
					Align: "center",
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "vault_use_" + it.ID,
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:       it.Name,
							Variant:     "secondary",
							Grow:        true,
							Height:      30,
							Tooltip:     "Click to place or drag onto page",
							DragPayload: "vault:" + it.ID,
							DragImage:   ref,
							DragW:       130,
							DragH:       52,
						}),
					},
					{
						ID:   "vault_del_" + it.ID,
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Icon:     "trash",
							IconOnly: true,
							Variant:  "ghost",
							Width:    30,
							Height:   30,
							Tooltip:  s.T("pdfsign.delete"),
						}),
					},
				},
			})

			sectionChildren = append(sectionChildren, pluginapi.UINode{
				ID:   "vault_card_" + it.ID,
				Kind: pluginapi.UINodeColumn,
				Props: mustProps(pluginapi.UIContainerProps{
					Padding:    8,
					Gap:        6,
					Surface:    "ground",
					BorderSize: 1,
				}),
				Children: cardChildren,
			})
		}
	}

	return pluginapi.UINode{
		ID:   "vault_section",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Gap: 8,
		}),
		Children: sectionChildren,
	}
}

func (s *pdfSignServer) buildActiveInspectorLocked() *pluginapi.UINode {
	var active *pdfsign.PlacedSignature
	for i := range s.placements {
		if s.placements[i].ID == s.activeID {
			active = &s.placements[i]
			break
		}
	}
	if active == nil {
		return nil
	}

	lockLabel := "Unlocked (Free)"
	if s.lockAspect {
		lockLabel = "Locked Aspect"
	}
	node := pluginapi.UINode{
		ID:   "active_inspector",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Padding:     12,
			Gap:         10,
			Surface:     "ground",
			BorderSize:  2,
			BorderToken: "accent",
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "insp_hdr",
				Kind: pluginapi.UINodeText,
				Props: mustProps(pluginapi.UITextProps{
					Text:       fmt.Sprintf("Selected Signature (Page %d)", active.Page),
					FontSize:   13,
					FontWeight: 700,
				}),
			},
			{
				ID:   "insp_coords",
				Kind: pluginapi.UINodeText,
				Props: mustProps(pluginapi.UITextProps{
					Text:        fmt.Sprintf("%.0fx%.0f pt · %.0f°", active.Width, active.Height, active.Rotation),
					Variant:     "muted",
					TabularNums: true,
				}),
			},
			{
				ID:   "insp_ink_row",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Gap:  6,
					Wrap: true,
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "insp_ink_orig",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.ink_original"),
							Variant:  "secondary",
							Selected: active.InkMode == pdfsign.InkOriginal,
							Height:   28,
						}),
					},
					{
						ID:   "insp_ink_black",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.ink_black"),
							Variant:  "secondary",
							Selected: active.InkMode == pdfsign.InkBlack,
							Height:   28,
						}),
					},
					{
						ID:   "insp_ink_blue",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.ink_blue"),
							Variant:  "secondary",
							Selected: active.InkMode == pdfsign.InkBlue,
							Height:   28,
						}),
					},
				},
			},
			{
				ID:   "insp_act_row",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Gap:  6,
					Wrap: true,
				}),
				Children: []pluginapi.UINode{
					{
						ID:   "insp_lock",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    lockLabel,
							Variant:  "secondary",
							Selected: s.lockAspect,
							Height:   28,
						}),
					},
					{
						ID:   "insp_crisp",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:    s.T("pdfsign.crisp_ink"),
							Variant:  "secondary",
							Selected: active.Crisp,
							Height:   28,
						}),
					},
					{
						ID:   "insp_rot_right",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:   s.T("pdfsign.rotate_right"),
							Variant: "secondary",
							Height:  28,
						}),
					},
					{
						ID:   "insp_all_pages",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:   s.T("pdfsign.btn_all_pages"),
							Variant: "secondary",
							Height:  28,
						}),
					},
					{
						ID:   "insp_del",
						Kind: pluginapi.UINodeButton,
						Props: mustProps(pluginapi.UIButtonProps{
							Label:   s.T("pdfsign.delete"),
							Variant: "danger",
							Height:  28,
						}),
					},
				},
			},
		},
	}
	return &node
}

func (s *pdfSignServer) buildPageJumpSectionLocked() pluginapi.UINode {
	chips := make([]pluginapi.UINode, 0, min(s.pageCount, 24))
	limit := min(s.pageCount, 24)
	for p := 1; p <= limit; p++ {
		chips = append(chips, pluginapi.UINode{
			ID:   fmt.Sprintf("rail_page_%d", p),
			Kind: pluginapi.UINodeButton,
			Props: mustProps(pluginapi.UIButtonProps{
				Label:    fmt.Sprintf("%d", p),
				Variant:  "secondary",
				Selected: p == s.currentPage,
				Width:    36,
				Height:   28,
			}),
		})
	}
	return pluginapi.UINode{
		ID:   "page_jump_section",
		Kind: pluginapi.UINodeColumn,
		Props: mustProps(pluginapi.UIContainerProps{
			Padding:    12,
			Gap:        8,
			Surface:    "ground",
			BorderSize: 1,
		}),
		Children: []pluginapi.UINode{
			{
				ID:   "page_jump_hdr",
				Kind: pluginapi.UINodeText,
				Props: mustProps(pluginapi.UITextProps{
					Text:       s.T("pdfsign.sidebar_pages"),
					FontSize:   13,
					FontWeight: 700,
				}),
			},
			{
				ID:   "page_jump_grid",
				Kind: pluginapi.UINodeRow,
				Props: mustProps(pluginapi.UIContainerProps{
					Gap:  6,
					Wrap: true,
				}),
				Children: chips,
			},
		},
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
