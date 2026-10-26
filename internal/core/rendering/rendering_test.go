package rendering

import (
	"errors"
	"testing"
)

// Requirement: RND-001
func TestRenderProfileIsVersionedCodecSafeAndResourceBounded(t *testing.T) {
	profile, err := NewProfile(Profile{
		ID: " Archive-Event-1080P ", Version: 1,
		Container: "MP4", VideoEncoder: "LIBX264", AudioEncoder: "AAC", PixelFormat: "YUV420P",
		MaxWidth: 1920, MaxHeight: 1080, ConstantRateFactor: 20, AudioBitRateKbps: 192, ThreadCount: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "archive-event-1080p" || profile.Reference() != "archive-event-1080p@v1" || profile.ThreadCount != 4 {
		t.Fatalf("profile was not normalized with a stable version: %+v", profile)
	}

	invalid := []Profile{
		func() Profile { value := profile; value.Version = 0; return value }(),
		func() Profile { value := profile; value.MaxWidth = 1919; return value }(),
		func() Profile { value := profile; value.ConstantRateFactor = 52; return value }(),
		func() Profile { value := profile; value.ThreadCount = 17; return value }(),
	}
	for _, candidate := range invalid {
		if _, err := NewProfile(candidate); !errors.Is(err, ErrInvalidProfile) {
			t.Fatalf("unsafe resource profile accepted: %+v err=%v", candidate, err)
		}
	}
	unsupported := profile
	unsupported.VideoEncoder = "arbitrary_encoder"
	if _, err := NewProfile(unsupported); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("unapproved encoder accepted: %v", err)
	}
}
