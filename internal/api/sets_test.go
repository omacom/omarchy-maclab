package api

import (
	"strings"
	"testing"
)

func TestParseSetItem(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for in, want := range map[string]SetItem{
		"linux-aurora=build:b-12":               {Package: "linux-aurora", Source: SetBuild, Ref: "b-12"},
		"mesa-aurora=file:" + sha:               {Package: "mesa-aurora", Source: SetFile, Ref: sha},
		"mesa-aurora=file:" + sha + "@25.3.0-1": {Package: "mesa-aurora", Source: SetFile, Ref: sha, Version: "25.3.0-1"},
		"m1n1-aurora=repo:1.5.2-1":              {Package: "m1n1-aurora", Source: SetRepo, Ref: "1.5.2-1", Version: "1.5.2-1"},
	} {
		got, err := ParseSetItem(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "linux-aurora", "linux-aurora=build:", "x=ftp:1", "=repo:1", "Bad Name=repo:1", "m=file:abc"} {
		if _, err := ParseSetItem(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

func TestStackPackage(t *testing.T) {
	for _, p := range []string{"linux-aurora", "linux-aurora-headers", "m1n1-aurora", "uboot-asahi", "mesa", "mesa-aurora", "vulkan-asahi", "vulkan-icd-loader", "libva-v4l2_request-avd", "libfprint", "asahi-fwextract", "asahi-firmware", "linux-firmware-asahi"} {
		if !StackPackage(p) {
			t.Errorf("%s is not part of the stack", p)
		}
	}
	for _, p := range []string{"linux", "firefox", "bash", "libva", "linux-firmware", "m1n1x", "omarchy-mlx", "ffmpeg"} {
		if StackPackage(p) {
			t.Errorf("%s is part of the stack", p)
		}
	}
}

// A Mac reports the stack and, by name, what a Performance run records it
// booted; capture keeps to the stack.
func TestBootedPackage(t *testing.T) {
	for _, p := range []string{"mesa-aurora", "libane", "omarchy-ane", "omarchy-ane-dkms", "mil-hwx-compiler", "omarchy-mlx", "omarchy-mlx-vulkan", "avd-fw", "ffmpeg", "libva", "ocl-icd", "opencl-aurora"} {
		if !BootedPackage(p) {
			t.Errorf("%s is not reported", p)
		}
	}
	for _, p := range []string{"bash", "python", "ffmpeg4.4", "omarchy", "libane-tools"} {
		if BootedPackage(p) {
			t.Errorf("%s is reported", p)
		}
	}
}

func TestSetName(t *testing.T) {
	for _, ok := range []string{"7.2", "mesa-25.3_try", "a"} {
		if err := CheckSetName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "A B", "a/b", string(make([]byte, 65))} {
		if CheckSetName(bad) == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}
