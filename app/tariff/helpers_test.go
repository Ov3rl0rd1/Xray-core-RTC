package tariff

import (
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeStateVersion writes a state file that claims the given version, which is
// what a file written by a newer build looks like from disk.
//
// It builds the file through protojson rather than by patching the text of a
// real one: protojson deliberately varies the whitespace it emits, so any test
// that matched its output byte for byte would pass or fail at random.
func writeStateVersion(t *testing.T, path string, version int64) {
	t.Helper()
	raw, err := protojson.Marshal(&State{Version: version})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(raw))
}
