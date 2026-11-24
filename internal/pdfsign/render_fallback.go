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
	"os/exec"
	"strconv"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

const maxFallbackRenderBytes = 32 << 20 // 32 MiB stdout cap for pdftoppm

type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.limit {
		l.exceeded = true
		return 0, errors.New("render output exceeded limit")
	}
	return l.buf.Write(p)
}

// FallbackRenderer provides PDF dimension parsing and image rendering on non-macOS or headless platforms.
type FallbackRenderer struct{}

func (r *FallbackRenderer) PageCount(ctx context.Context, pdfPath string) (int, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return 0, fmt.Errorf("open pdf %s: %w", pdfPath, err)
	}
	defer f.Close()
	return api.PageCount(ctx, f, model.NewStatelessConfiguration())
}

// AllPageDimensions reads every page's visual dimensions in points in a single pdfcpu pass.
func (r *FallbackRenderer) AllPageDimensions(ctx context.Context, pdfPath string) ([]PageDim, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return nil, fmt.Errorf("open pdf %s: %w", pdfPath, err)
	}
	defer f.Close()
	dims, err := api.PageDims(ctx, f, model.NewStatelessConfiguration())
	if err != nil {
		return nil, fmt.Errorf("read pdf dimensions %s: %w", pdfPath, err)
	}
	out := make([]PageDim, len(dims))
	for i, d := range dims {
		out[i] = PageDim{Width: d.Width, Height: d.Height}
	}
	return out, nil
}

func (r *FallbackRenderer) PageDimensions(ctx context.Context, pdfPath string, pageNum int) (float64, float64, error) {
	if pageNum < 1 {
		return 0, 0, ErrPageOutOfRange
	}
	dims, err := r.AllPageDimensions(ctx, pdfPath)
	if err != nil {
		return 0, 0, err
	}
	if pageNum > len(dims) {
		return 0, 0, ErrPageOutOfRange
	}
	d := dims[pageNum-1]
	return d.Width, d.Height, nil
}

func (r *FallbackRenderer) RenderPage(ctx context.Context, pdfPath string, pageNum int, scale float64) (image.Image, error) {
	if scale <= 0 {
		scale = 1.0
	}

	if pdftoppmPath, err := exec.LookPath("pdftoppm"); err == nil {
		dpi := int(math.Round(72.0 * scale))
		if dpi < 36 {
			dpi = 36
		}
		if dpi > 300 {
			dpi = 300
		}
		cmd := exec.CommandContext(ctx, pdftoppmPath,
			"-png",
			"-f", strconv.Itoa(pageNum),
			"-l", strconv.Itoa(pageNum),
			"-r", strconv.Itoa(dpi),
			pdfPath,
			"-",
		)
		setCmdHideWindow(cmd)
		out := &limitedBuffer{limit: maxFallbackRenderBytes}
		cmd.Stdout = out
		if err := cmd.Run(); err == nil && !out.exceeded {
			if img, err := png.Decode(&out.buf); err == nil {
				return img, nil
			}
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	if _, _, err := r.PageDimensions(ctx, pdfPath, pageNum); err != nil {
		return nil, err
	}
	return nil, ErrNoPreviewEngine
}

var (
	_ PageRenderer           = (*FallbackRenderer)(nil)
	_ BatchDimensionRenderer = (*FallbackRenderer)(nil)
)
