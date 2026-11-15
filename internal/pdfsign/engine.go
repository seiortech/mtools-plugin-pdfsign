package pdfsign

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var (
	ErrEmptyInput       = errors.New("no input or output path specified")
	ErrNoSignatures     = errors.New("no signature placements provided")
	ErrInvalidPageNum   = errors.New("signature placed on invalid page number")
	ErrZeroSizeSignature = errors.New("signature dimensions must be greater than zero")
)

// PlacedSignature defines a signature positioned on a specific PDF page.
type PlacedSignature struct {
	ID        string          // Unique placement ID
	SourceID  string          // Vault signature ID (if from vault)
	Page      int             // 1-based PDF page number
	X         float64         // X position in PDF points from top-left
	Y         float64         // Y position in PDF points from top-left
	Width     float64         // Width in PDF points
	Height    float64         // Height in PDF points
	Rotation  float64         // Rotation in degrees (0, 90, 180, 270)
	Opacity   float64         // Opacity: 0.0 to 1.0 (defaults to 1.0)
	Crop      NormalizedRect  // Optional sub-crop [0..1]
	InkMode   InkColorMode    // Ink recoloring (InkOriginal, InkBlack, InkBlue, InkRed)
	Image     image.Image     // Source raster image
	Locked    bool            // If true, placement cannot be moved, resized, rotated, or deleted
	Crisp     bool            // If true, applies EnhanceSignatureInk for sharper edges and smooth lines

	// Cached rendered image & bitmap
	cachedRot   float64
	cachedOp    float64
	cachedInk   InkColorMode
	cachedCrop  NormalizedRect
	cachedImg   image.Image
	cachedBmp   any // *ui.Bitmap stored as any to avoid import cycles
}

// SignRequest contains all parameters required to sign a PDF.
type SignRequest struct {
	InFile     string
	OutFile    string
	Signatures []PlacedSignature
}

// SignPDF applies the requested signature placements onto the PDF.
// All operations use stateless pdfcpu configuration and stage writes atomically.
func SignPDF(ctx context.Context, req SignRequest, progress func(done float64, msg string)) error {
	if req.InFile == "" || req.OutFile == "" {
		return ErrEmptyInput
	}
	if len(req.Signatures) == 0 {
		return ErrNoSignatures
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if progress != nil {
		progress(0.05, "Reading PDF layout...")
	}

	f, err := os.Open(req.InFile)
	if err != nil {
		return fmt.Errorf("open input pdf: %w", err)
	}
	defer f.Close()

	conf := model.NewStatelessConfiguration()
	pdfCtx, err := api.ReadAndValidate(ctx, f, conf)
	if err != nil {
		return fmt.Errorf("read pdf context: %w", err)
	}
	pageDims, err := pdfCtx.PageDims(ctx)
	if err != nil {
		return fmt.Errorf("read page dimensions: %w", err)
	}
	if len(pageDims) == 0 {
		return fmt.Errorf("pdf has no pages")
	}

	type stampedSig struct {
		xobjName string
		imgRef   *types.IndirectRef
		drawnW   float64
		drawnH   float64
		offsetX  float64
		offsetY  float64
	}

	pageStamps := make(map[int][]stampedSig)
	totalSigs := float64(len(req.Signatures))

	for i, sig := range req.Signatures {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sig.Page < 1 || sig.Page > len(pageDims) {
			return fmt.Errorf("%w: page %d (total %d)", ErrInvalidPageNum, sig.Page, len(pageDims))
		}
		if sig.Width <= 0 || sig.Height <= 0 {
			return ErrZeroSizeSignature
		}
		if sig.Image == nil {
			return fmt.Errorf("signature image is nil for placement %s", sig.ID)
		}

		if progress != nil {
			p := 0.10 + (float64(i)/totalSigs)*0.40
			progress(p, fmt.Sprintf("Rendering signature %d of %d...", i+1, len(req.Signatures)))
		}

		// 1. Apply ink color if specified
		procImg := sig.Image
		if sig.InkMode != InkOriginal {
			procImg = RecolorSignature(procImg, sig.InkMode)
		}

		// 2. Render placement with crop, rotation, opacity
		opacity := sig.Opacity
		if opacity <= 0 || opacity > 1.0 {
			opacity = 1.0
		}
		crop := sig.Crop
		if crop.W <= 0 || crop.H <= 0 {
			crop = DefaultNormalizedRect()
		}
		if sig.Crisp {
			procImg = EnhanceSignatureInk(procImg)
		}

		compImg := RenderPlacementImage(procImg, PlacementRenderOptions{
			Crop:        crop,
			RotationDeg: sig.Rotation,
			Opacity:     opacity,
		})

		// 3. Encode composited PNG to buffer
		var buf bytes.Buffer
		if err := png.Encode(&buf, compImg); err != nil {
			return fmt.Errorf("encode placement image: %w", err)
		}

		// 4. Calculate scale factor and position matching ui.Image Contain fit.
		imgW := float64(compImg.Bounds().Dx())
		imgH := float64(compImg.Bounds().Dy())
		if imgW <= 0 || imgH <= 0 {
			continue
		}

		scaleFactor := math.Min(sig.Width/imgW, sig.Height/imgH)
		if scaleFactor <= 0 {
			continue
		}
		drawnW := imgW * scaleFactor
		drawnH := imgH * scaleFactor
		offsetX := sig.X + (sig.Width-drawnW)/2
		offsetY := sig.Y + (sig.Height-drawnH)/2

		// 5. Embed PNG directly as a PDF Image XObject (bypasses pdfcpu watermark DSL/DiagonalLLToUR).
		imgRef, _, _, err := model.CreateImageResource(pdfCtx.XRefTable, &buf)
		if err != nil {
			return fmt.Errorf("create image xobject: %w", err)
		}

		pageStamps[sig.Page] = append(pageStamps[sig.Page], stampedSig{
			xobjName: fmt.Sprintf("MToolsSig%d", i+1),
			imgRef:   imgRef,
			drawnW:   drawnW,
			drawnH:   drawnH,
			offsetX:  offsetX,
			offsetY:  offsetY,
		})
	}

	if progress != nil {
		progress(0.60, "Stamping signatures onto PDF...")
	}

	pageNums := make([]int, 0, len(pageStamps))
	for pg := range pageStamps {
		pageNums = append(pageNums, pg)
	}
	sort.Ints(pageNums)

	for _, pageNum := range pageNums {
		if err := ctx.Err(); err != nil {
			return err
		}
		d, _, attrs, err := pdfCtx.PageDict(ctx, pageNum, false)
		if err != nil {
			return fmt.Errorf("page %d dictionary: %w", pageNum, err)
		}
		if d == nil || attrs == nil {
			return fmt.Errorf("page %d missing dictionary or attributes", pageNum)
		}

		vp := attrs.CropBox
		if vp == nil {
			vp = attrs.MediaBox
		}
		if vp == nil {
			dim := pageDims[pageNum-1]
			vp = types.RectForDim(dim.Width, dim.Height)
		}

		pageRot := ((attrs.Rotate % 360) + 360) % 360
		if pageRot != 0 {
			if pageRot == 90 || pageRot == 270 {
				w := vp.Width()
				vp = types.NewRectangle(vp.LL.X, vp.LL.Y, vp.LL.X+vp.Height(), vp.LL.Y+w)
			}
			d.Update("MediaBox", vp.Array())
			d.Update("CropBox", vp.Array())
			d.Delete("Rotate")
		}

		// Attach XObjects to page /Resources
		resDict := attrs.Resources
		if resDict == nil {
			resDict = types.NewDict()
		}
		var xobjDict types.Dict
		if obj, found := resDict.Find("XObject"); found {
			deref, err := pdfCtx.DereferenceDict(obj)
			if err != nil {
				return fmt.Errorf("page %d xobject resources: %w", pageNum, err)
			}
			xobjDict = deref
		}
		if xobjDict == nil {
			xobjDict = types.NewDict()
		}
		for _, s := range pageStamps[pageNum] {
			xobjDict.Insert(s.xobjName, *s.imgRef)
		}
		resDict.Insert("XObject", xobjDict)
		d.Update("Resources", resDict)

		// Build explicit zero-rotation PDF CTM commands in visual bottom-left coordinates.
		var stampBuf bytes.Buffer
		for _, s := range pageStamps[pageNum] {
			pdfX := vp.LL.X + s.offsetX
			pdfY := vp.LL.Y + vp.Height() - (s.offsetY + s.drawnH)
			fmt.Fprintf(&stampBuf, "q %.5f 0 0 %.5f %.5f %.5f cm /%s Do Q\n",
				s.drawnW, s.drawnH, pdfX, pdfY, s.xobjName)
		}

		if err := appendIsolatedPageOverlay(pdfCtx, d, pageRot, vp.Width(), vp.Height(), stampBuf.Bytes()); err != nil {
			return fmt.Errorf("page %d overlay: %w", pageNum, err)
		}
	}

	pdfCtx.EnsureVersionForWriting()

	// 6. Stage write in destination directory
	outDir := filepath.Dir(req.OutFile)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	stagingDir, err := os.MkdirTemp(outDir, ".mtools-pdfsign-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	tempFile := filepath.Join(stagingDir, "signed.pdf")
	if err := api.WriteContextFile(ctx, pdfCtx, tempFile); err != nil {
		return fmt.Errorf("stamp pdf: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if progress != nil {
		progress(0.95, "Finalizing output...")
	}

	// Atomic rename to target output path
	if err := os.Rename(tempFile, req.OutFile); err != nil {
		return fmt.Errorf("finalize signed output: %w", err)
	}

	if progress != nil {
		progress(1.0, "Signing complete!")
	}
	return nil
}

func appendIsolatedPageOverlay(pdfCtx *model.Context, pageDict types.Dict, pageRot int, vpW, vpH float64, overlayBytes []byte) error {
	if len(overlayBytes) == 0 {
		return nil
	}

	newStreamRef := func(data []byte) (*types.IndirectRef, error) {
		sd, err := pdfCtx.NewStreamDictForBuf(data)
		if err != nil {
			return nil, fmt.Errorf("create content stream: %w", err)
		}
		if err := sd.Encode(); err != nil {
			return nil, fmt.Errorf("encode content stream: %w", err)
		}
		ref, err := pdfCtx.IndRefForNewObject(*sd)
		if err != nil {
			return nil, fmt.Errorf("register content stream: %w", err)
		}
		return ref, nil
	}

	existingObj, hasContents := pageDict.Find("Contents")
	if !hasContents || existingObj == nil {
		stampRef, err := newStreamRef(overlayBytes)
		if err != nil {
			return err
		}
		pageDict.Insert("Contents", *stampRef)
		return nil
	}

	var prefix bytes.Buffer
	prefix.WriteString(" q ")
	if pageRot != 0 {
		prefix.Write(model.ContentBytesForPageRotation(pageRot, vpW, vpH))
	}
	prefixRef, err := newStreamRef(prefix.Bytes())
	if err != nil {
		return err
	}

	var suffix bytes.Buffer
	suffix.WriteString(" Q\n")
	suffix.Write(overlayBytes)
	stampRef, err := newStreamRef(suffix.Bytes())
	if err != nil {
		return err
	}

	var outArr types.Array
	outArr = append(outArr, *prefixRef)

	switch v := existingObj.(type) {
	case types.IndirectRef:
		resolved, err := pdfCtx.Dereference(v)
		if err != nil {
			outArr = append(outArr, v)
		} else if arr, ok := resolved.(types.Array); ok {
			outArr = append(outArr, arr...)
		} else {
			outArr = append(outArr, v)
		}
	case types.Array:
		outArr = append(outArr, v...)
	default:
		outArr = append(outArr, v)
	}

	outArr = append(outArr, *stampRef)
	pageDict.Update("Contents", outArr)
	return nil
}
