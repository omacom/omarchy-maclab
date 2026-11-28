package reports

import (
	"regexp"
	"strings"
)

// imageRelease is one published Omarchy Mac image release, as its IMAGE
// record names it.
type imageRelease struct {
	PackageSet, Built, Lane, Tag, Revision string
}

// publishedImages are the releases whose installs reported before
// omarchy-m-test kept the lane: each found by its package set digest and
// build time, which a rebuild does not share.
var publishedImages = []imageRelease{
	{PackageSet: "08c81abcef4faf16d46eb82f84e6177ed9d4cd17fd7d5729822946178f93b44e", Built: "2026-11-05T04:46:43Z", Lane: "stable", Tag: "v4.5.0", Revision: "2"},
	{PackageSet: "88821ec92e35c1874657f3d66c30917341a261d8c472c61d4271d3df218efef3", Built: "2026-11-22T04:58:52Z", Lane: "edge"},
}

var reChannelSet = regexp.MustCompile(`\Aomarchy-([a-z]+)-[0-9a-f]+\z`)

// Release is the image release the build was installed from:
// "stable 4.5.0-r2 (2026.11.05)", "edge 2026.11.22"; "" when its record
// doesn't say and it is no published release the lab knows.
func (b *Build) Release() string {
	r := imageRelease{Built: b.fact("built"), Lane: b.fact("lane"), Tag: b.fact("release_tag"), Revision: b.fact("mac_revision")}
	if r.Lane == "" {
		if found := reChannelSet.FindStringSubmatch(b.fact("channel_set")); found != nil {
			r.Lane = found[1]
		}
	}
	if digest := b.fact("package_set_sha256"); digest != "" && r.Built != "" {
		for _, known := range publishedImages {
			if known.PackageSet == digest && known.Built == r.Built {
				r = known
			}
		}
	}
	if r.Lane == "" {
		return ""
	}
	day := ""
	if len(r.Built) >= 10 {
		day = strings.ReplaceAll(r.Built[:10], "-", ".")
	}
	if r.Tag == "" {
		return strings.TrimSpace(r.Lane + " " + day)
	}
	name := r.Lane + " " + strings.TrimPrefix(r.Tag, "v")
	if r.Revision != "" {
		name += "-r" + r.Revision
	}
	if day != "" {
		name += " (" + day + ")"
	}
	return name
}
