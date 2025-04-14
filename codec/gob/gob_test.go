package gob

import (
	"testing"

	"github.com/clakeboy/storm-rev/codec/internal"
)

func TestGob(t *testing.T) {
	internal.RoundtripTester(t, Codec)
}
