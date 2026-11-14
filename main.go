package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/seiortech/mtools-sdk/pluginapi"
	"github.com/seiortech/mtools-plugin-pdfsign/internal/pdfsign"
)

type pdfSignServer struct{}

type rawPlacement struct {
	ID        string  `json:"id"`
	Page      int     `json:"page"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Width     float64 `json:"width"`
	Height    float64 `json:"height"`
	Rotation  float64 `json:"rotation"`
	Opacity   float64 `json:"opacity"`
	Base64PNG string  `json:"base64_png"`
}

func (s *pdfSignServer) Inspect(ctx context.Context, p pluginapi.InspectParams) (pluginapi.InspectResult, error) {
	res := pluginapi.InspectResult{
		Fields:       make(map[string]pluginapi.InspectFieldUpdate),
		Metadata:     make(map[string]string),
		HelperStatus: make(map[string]pluginapi.HelperStatus),
	}

	if files, ok := p.Inputs["input_file"]; ok && len(files) > 0 && files[0] != "" {
		renderer := pdfsign.DefaultRenderer()
		if count, err := renderer.PageCount(ctx, files[0]); err == nil {
			res.Fields["input_file"] = pluginapi.InspectFieldUpdate{
				Info: fmt.Sprintf("%d pages", count),
			}
		}
	}

	return res, nil
}

func (s *pdfSignServer) Run(ctx context.Context, p pluginapi.RunParams, emit func(pluginapi.Progress) error) (pluginapi.RunResult, error) {
	if p.Tool != "pdfsign" {
		return pluginapi.RunResult{}, errors.New("unsupported tool")
	}

	switch p.Action {
	case "sign":
		inFiles := p.Inputs["input_file"]
		if len(inFiles) == 0 || inFiles[0] == "" {
			return pluginapi.RunResult{}, errors.New("missing input PDF")
		}
		inFile := inFiles[0]

		outFiles := p.Inputs["output_file"]
		if len(outFiles) == 0 || outFiles[0] == "" {
			return pluginapi.RunResult{}, errors.New("missing output file")
		}
		outFile := outFiles[0]

		var placements []pdfsign.PlacedSignature
		if rawJSON, ok := p.Inputs["placements"]; ok && len(rawJSON) > 0 && rawJSON[0] != "" {
			var rawList []rawPlacement
			if err := json.Unmarshal([]byte(rawJSON[0]), &rawList); err == nil {
				for _, r := range rawList {
					var img image.Image
					if r.Base64PNG != "" {
						data, err := base64.StdEncoding.DecodeString(r.Base64PNG)
						if err == nil {
							img, _ = png.Decode(strings.NewReader(string(data)))
						}
					}
					placements = append(placements, pdfsign.PlacedSignature{
						ID:       r.ID,
						Page:     r.Page,
						X:        r.X,
						Y:        r.Y,
						Width:    r.Width,
						Height:   r.Height,
						Rotation: r.Rotation,
						Opacity:  r.Opacity,
						Crop:     pdfsign.DefaultNormalizedRect(),
						InkMode:  pdfsign.InkOriginal,
						Image:    img,
					})
				}
			}
		}

		_ = emit(pluginapi.Progress{
			JobID:   p.JobID,
			Done:    10,
			Total:   100,
			Unit:    "%",
			Message: "Signing PDF document...",
			LogLine: fmt.Sprintf("Signing %s with %d placement(s)", filepath.Base(inFile), len(placements)),
		})

		req := pdfsign.SignRequest{
			InFile:     inFile,
			OutFile:    outFile,
			Signatures: placements,
		}

		err := pdfsign.SignPDF(ctx, req, func(done float64, msg string) {
			_ = emit(pluginapi.Progress{
				JobID:   p.JobID,
				Done:    done * 100,
				Total:   100,
				Unit:    "%",
				Message: msg,
				LogLine: msg,
			})
		})
		if err != nil {
			return pluginapi.RunResult{}, err
		}

		return pluginapi.RunResult{
			JobID:   p.JobID,
			Outputs: []string{outFile},
			Summary: fmt.Sprintf("Signed PDF saved to %s", filepath.Base(outFile)),
		}, nil

	default:
		return pluginapi.RunResult{}, fmt.Errorf("unknown action: %s", p.Action)
	}
}

func main() {
	identity := pluginapi.InitializeResult{
		Protocol: pluginapi.Protocol,
		PluginID: "com.mtools.pdfsign",
		Version:  "1.0.0",
	}
	server := &pdfSignServer{}
	if err := pluginapi.Serve(context.Background(), os.Stdin, os.Stdout, identity, server); err != nil {
		os.Exit(1)
	}
}
