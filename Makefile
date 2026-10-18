# goolm selects mautrix's pure-Go Olm implementation (no libolm needed).
GOFLAGS := -tags=goolm -buildvcs=false
export GOFLAGS
# Go 1.26+ defaults to encoding/json/v2, under which mautrix signs one-time
# keys incorrectly (other devices then can't establish Olm sessions).
GOEXPERIMENT := nojsonv2
export GOEXPERIMENT
# Some environments set GOROOT to an old Go tree; let the go binary choose.
unexport GOROOT

GO_LICENSES := github.com/google/go-licenses/v2@v2.0.1

.PHONY: build test vet notices
build:
	go build -o bin/slack2matrix ./cmd/slack2matrix

test:
	go test ./...

vet:
	go vet ./...

# THIRD_PARTY_NOTICES.md: the licenses of the Go modules compiled into the
# binary, plus components go-licenses can't see (scripts/notices-extra.md).
notices:
	go run $(GO_LICENSES) report ./cmd/slack2matrix --ignore slack2matrix \
		--template scripts/notices.tpl > THIRD_PARTY_NOTICES.md.tmp
	cat scripts/notices-extra.md >> THIRD_PARTY_NOTICES.md.tmp
	mv THIRD_PARTY_NOTICES.md.tmp THIRD_PARTY_NOTICES.md
