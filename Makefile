# goolm selects mautrix's pure-Go Olm implementation (no libolm needed).
GOFLAGS := -tags=goolm -buildvcs=false
export GOFLAGS
# Go 1.26+ defaults to encoding/json/v2, under which mautrix signs one-time
# keys incorrectly (other devices then can't establish Olm sessions).
GOEXPERIMENT := nojsonv2
export GOEXPERIMENT
# Some environments set GOROOT to an old Go tree; let the go binary choose.
unexport GOROOT

.PHONY: build test vet
build:
	go build -o bin/slack2matrix ./cmd/slack2matrix

test:
	go test ./...

vet:
	go vet ./...
