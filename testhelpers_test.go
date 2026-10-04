package flagengine

import (
	"os"
	"strings"
	"testing"
)

func readFileForTest(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var _ = strings.Contains
