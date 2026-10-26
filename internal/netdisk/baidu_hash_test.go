package netdisk

import "testing"

func TestBaiduMD5(t *testing.T) {
	const plain = "d41d8cd98f00b204e9800998ecf8427e"
	for _, raw := range []string{plain, "D41D8CD98F00B204E9800998ECF8427E", "8e23f7635tb64136eddb0719602bc477", "8E23F7635TB64136EDDB0719602BC477"} {
		got, err := BaiduMD5(raw)
		if err != nil || got != plain {
			t.Fatalf("digest conversion failed: %q, %v", got, err)
		}
	}
	for _, raw := range []string{"", "short", "8e23f7635wb64136eddb0719602bc477", "ze23f7635tb64136eddb0719602bc477"} {
		if _, err := BaiduMD5(raw); err == nil {
			t.Fatal("malformed digest accepted")
		}
	}
}
