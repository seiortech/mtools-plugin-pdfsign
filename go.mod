module github.com/seiortech/mtools-plugin-pdfsign

go 1.27.3

require (
	github.com/ebitengine/purego v0.11.1
	github.com/egoist/mygo v0.9.0
	github.com/pdfcpu/pdfcpu v0.16.4
	github.com/seiortech/mtools-sdk v0.1.0
	github.com/seiortech/mtools-sdk/ui v0.1.0
)

require (
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/go-text/typesetting v0.3.5 // indirect
	github.com/hhrutter/tiff v1.0.7 // indirect
	github.com/mattn/go-runewidth v0.0.31 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/image v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace (
	github.com/seiortech/mtools-sdk => ../mtools-sdk
	github.com/seiortech/mtools-sdk/ui => ../mtools-sdk/ui
)
