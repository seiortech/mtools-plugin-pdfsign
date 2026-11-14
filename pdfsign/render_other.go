//go:build !darwin

package pdfsign

func defaultPlatformRenderer() PageRenderer {
	return &FallbackRenderer{}
}
