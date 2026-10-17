package e2ee

import "testing"

// The Makefile builds with GOEXPERIMENT=nojsonv2; this fails under json/v2.
func TestCanonicalJSONSignsOneTimeKeysCorrectly(t *testing.T) {
	if err := checkCanonicalJSON(); err != nil {
		t.Fatal(err)
	}
}
