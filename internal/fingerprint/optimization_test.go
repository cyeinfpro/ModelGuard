package fingerprint

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
)

func TestDecodeRejectsMissingOrMismatchedCalibration(t *testing.T) {
	n := int(binary.LittleEndian.Uint32(encoded[:4]))
	var original Artifact
	if err := json.Unmarshal(encoded[4:4+n], &original); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing", "hash", "order", "tau", "empty_binding", "invalid_hash"} {
		t.Run(kind, func(t *testing.T) {
			var a Artifact
			if err := json.Unmarshal(encoded[4:4+n], &a); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "empty_binding":
				a.BaseSHA256 = ""
				a.Calibration.Binding.BaseSHA256 = ""
			case "invalid_hash":
				a.SourceReferenceSHA256 = "invalid"
				a.Calibration.Binding.ReferenceSHA256 = "invalid"
			case "missing":
				a.Calibration = nil
			case "hash":
				a.Calibration.Binding.BaseSHA256 = "wrong"
			case "order":
				a.Calibration.Binding.ModelIDs[0] = "wrong"
			case "tau":
				a.Calibration.Tau = 0
			}
			header, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			pos := (4 + len(header) + 3) &^ 3
			oldPos := (4 + n + 3) &^ 3
			data := make([]byte, pos+len(encoded)-oldPos)
			binary.LittleEndian.PutUint32(data[:4], uint32(len(header)))
			copy(data[4:], header)
			copy(data[pos:], encoded[oldPos:])
			if _, err := Decode(data, encodedMeta); err == nil {
				t.Fatal("invalid calibration accepted")
			}
		})
	}
	original.Calibration.Tau = math.NaN()
	if err := validateCalibration(original); err == nil {
		t.Fatal("NaN calibration accepted")
	}
}
func FuzzParseNumbers(f *testing.F) {
	f.Add("1,2,3")
	f.Add("hello 9999999999999999999999999999 世界 4")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<16 {
			t.Skip()
		}
		for _, n := range parseNumbers(raw) {
			if n < 1 || n > 1000 {
				t.Fatalf("invalid parsed number %d", n)
			}
		}
	})
}
