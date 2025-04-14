package json

import (
	"testing"

	"github.com/clakeboy/storm-rev/codec/internal"
)

func TestJSON(t *testing.T) {
	internal.RoundtripTester(t, Codec)
}
