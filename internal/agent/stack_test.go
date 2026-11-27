package agent

import (
	"maps"
	"testing"
)

// The stack a Mac reports is pacman -Q's lines for the stack's packages only.
func TestStackOfPacmanQ(t *testing.T) {
	out := "bash 5.2.037-5\nlinux-aurora 6.17.5.aurora1-1\nlinux-aurora-headers 6.17.5.aurora1-1\nm1n1-aurora 1.5.2-1\n" +
		"mesa 1:25.3.0-1\nfirefox 145.0-1\nuboot-asahi 2025.10-1\nasahi-fwextract 0.7.9-1\nnot a line\n"
	want := map[string]string{"linux-aurora": "6.17.5.aurora1-1", "linux-aurora-headers": "6.17.5.aurora1-1",
		"m1n1-aurora": "1.5.2-1", "mesa": "1:25.3.0-1", "uboot-asahi": "2025.10-1", "asahi-fwextract": "0.7.9-1"}
	if got := stackOf([]byte(out)); !maps.Equal(got, want) {
		t.Fatalf("stack %v, want %v", got, want)
	}
	// The Neural engine's driver, libane, MLX and llama.cpp go by their
	// names, never by a part of one; so does what a Performance run records
	// it booted, by Omarchy's package names.
	out = "libane 0.2.0-1\nane-dkms 0.2.0-1\npython-mlx 0.29.1-1\nllama.cpp-vulkan b6500-1\n" +
		"omarchy-ane 0.5.6-1\nomarchy-ane-dkms 0.5.7-1\nmil-hwx-compiler 0.1.0-1\nomarchy-mlx 0.7.28-5\nomarchy-mlx-vulkan 0.7.28-5\n" +
		"avd-fw 0.1-1\nffmpeg 2:9.0.2-2\nlibva 2.24.1-1\nocl-icd 2.3.5-1\n" +
		"xfce4-panel 4.20.4-1\nplanetary 1-1\nlibmlx-tools 1-1\nllama-tools 1-1\nffmpegthumbnailer 2.2.3-1\n"
	want = map[string]string{"libane": "0.2.0-1", "ane-dkms": "0.2.0-1", "python-mlx": "0.29.1-1", "llama.cpp-vulkan": "b6500-1",
		"omarchy-ane": "0.5.6-1", "omarchy-ane-dkms": "0.5.7-1", "mil-hwx-compiler": "0.1.0-1", "omarchy-mlx": "0.7.28-5",
		"omarchy-mlx-vulkan": "0.7.28-5", "avd-fw": "0.1-1", "ffmpeg": "2:9.0.2-2", "libva": "2.24.1-1", "ocl-icd": "2.3.5-1"}
	if got := stackOf([]byte(out)); !maps.Equal(got, want) {
		t.Fatalf("stack %v, want %v", got, want)
	}
	if got := stackOf(nil); got != nil {
		t.Fatalf("no pacman: %v", got)
	}
}
