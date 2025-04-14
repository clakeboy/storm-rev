package aes

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/clakeboy/storm-rev/codec/internal"
	"github.com/clakeboy/storm-rev/codec/json"
)

var testKey, _ = base64.StdEncoding.DecodeString("xkBTXc1wn0C/aL31u9SA7g==")

func TestAES(t *testing.T) {
	aes, err := NewAES(json.Codec, testKey)
	require.NoError(t, err)

	internal.RoundtripTester(t, aes)
}
