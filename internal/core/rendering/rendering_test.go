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

// Requirement: RND-004
func TestOrganizationRenderProfilesAllowOnlySixOrTwentyMbpsAndDefaultToSix(t *testing.T) {
	defaultProfile, err := OrganizationProfile(0)
	if err != nil {
		t.Fatal(err)
	}
	six, err := OrganizationProfile(6_000)
	if err != nil {
		t.Fatal(err)
	}
	twenty, err := OrganizationProfile(20_000)
	if err != nil {
		t.Fatal(err)
	}
	if defaultProfile != six || six.Reference() != "archive-1080p-6mbps@v1" || six.TargetVideoBitRateKbps != 6_000 ||
		twenty.Reference() != "archive-1080p-20mbps@v1" || twenty.TargetVideoBitRateKbps != 20_000 ||
		six.MaxWidth != 1920 || six.MaxHeight != 1080 || twenty.MaxWidth != 1920 || twenty.MaxHeight != 1080 {
		t.Fatalf("organization profiles changed: default=%+v six=%+v twenty=%+v", defaultProfile, six, twenty)
	}
	if _, err := OrganizationProfile(8_000); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("arbitrary organization bitrate accepted: %v", err)
	}
}
