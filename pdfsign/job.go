package pdfsign

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/seiortech/mtools-sdk/job"
)

// SignJob executes PDF signature stamping in a background worker goroutine.
type SignJob struct {
	InFile     string
	OutFile    string
	Signatures []PlacedSignature
}

// Run implements job.Job.
func (j SignJob) Run(ctx context.Context, progress func(job.Progress)) (job.Result, error) {
	if progress == nil {
		progress = func(job.Progress) {}
	}

	progress(job.Progress{
		Done:    5,
		Total:   100,
		Unit:    "%",
		Message: "Starting PDF signing...",
	})

	select {
	case <-ctx.Done():
		return job.Result{}, job.ErrCanceled
	default:
	}

	req := SignRequest{
		InFile:     j.InFile,
		OutFile:    j.OutFile,
		Signatures: j.Signatures,
	}

	err := SignPDF(ctx, req, func(done float64, msg string) {
		pct := int(done * 100)
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		progress(job.Progress{
			Done:    float64(pct),
			Total:   100,
			Unit:    "%",
			Message: msg,
		})
	})

	if err != nil {
		if ctx.Err() != nil {
			return job.Result{}, job.ErrCanceled
		}
		return job.Result{}, err
	}

	summary := fmt.Sprintf("Signed %s with %d signature(s)", filepath.Base(j.OutFile), len(j.Signatures))
	return job.Result{
		Outputs: []string{j.OutFile},
		Summary: summary,
	}, nil
}
