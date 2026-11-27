package reports

import "testing"

func TestAnImageNamesTheReleaseItWasInstalledFrom(t *testing.T) {
	stable := "08c81abcef4faf16d46eb82f84e6177ed9d4cd17fd7d5729822946178f93b44e"
	edge := "88821ec92e35c1874657f3d66c30917341a261d8c472c61d4271d3df218efef3"
	for _, c := range []struct {
		name  string
		image []ImageFact
		want  string
	}{
		{"its record names it", []ImageFact{{"lane", "stable"}, {"release_tag", "v4.6.0"}, {"mac_revision", "1"}, {"built", "2026-12-01T01:02:03Z"}}, "stable 4.6.0-r1 (2026.12.01)"},
		{"its channel set names the lane", []ImageFact{{"channel_set", "omarchy-edge-64cd05520a11"}, {"built", "2026-12-02T01:02:03Z"}}, "edge 2026.12.02"},
		{"a published stable release", []ImageFact{{"package_set_sha256", stable}, {"built", "2026-11-05T04:46:43Z"}}, "stable 4.5.0-r2 (2026.11.05)"},
		{"a published edge release", []ImageFact{{"package_set_sha256", edge}, {"built", "2026-11-22T04:58:52Z"}}, "edge 2026.11.22"},
		{"a rebuild of the same package set", []ImageFact{{"package_set_sha256", stable}, {"built", "2026-11-06T00:00:00Z"}}, ""},
		{"an older image", []ImageFact{{"format", "1"}, {"platform", "apple-silicon"}}, ""},
	} {
		if got := NewBuild(map[string]string{"omarchy": "4.5.0-3"}, "", c.image, "0.2.0").Release(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
