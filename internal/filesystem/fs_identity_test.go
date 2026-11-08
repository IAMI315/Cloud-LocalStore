package filesystem

import "testing"

func TestStatPlatformIdentityIsOptional(t *testing.T) {
	info, err := (OSFileSystem{}).Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// PlatformID is a best-effort hint. Empty is valid on unsupported filesystems.
	if info.PlatformID == "" {
		t.Log("platform did not expose a stable file identity")
	}
}
