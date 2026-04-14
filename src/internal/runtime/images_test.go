package runtime

import "testing"

func TestChooseBucketUsesOfficialAspectBuckets(t *testing.T) {
	tests := []struct {
		width  int
		height int
		wantW  int
		wantH  int
	}{
		{width: 4032, height: 3024, wantW: 1152, wantH: 864},
		{width: 3024, height: 4032, wantW: 864, wantH: 1152},
		{width: 4000, height: 2250, wantW: 1280, wantH: 720},
		{width: 2250, height: 4000, wantW: 720, wantH: 1280},
	}

	for _, tt := range tests {
		got := chooseBucket(tt.width, tt.height)
		if got.width != tt.wantW || got.height != tt.wantH {
			t.Fatalf("chooseBucket(%d, %d) = %dx%d, want %dx%d", tt.width, tt.height, got.width, got.height, tt.wantW, tt.wantH)
		}
	}
}
