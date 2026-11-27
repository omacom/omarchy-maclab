package api

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A Package set is a frozen list of packages, by name, each from one source:
// a version from the Mac's own repositories, a file uploaded with lab upload,
// or a Builder build. Every file in it is named by its sha256. A set is never
// changed once made: a new one is made instead.
type PackageSet struct {
	Name      string    `json:"name"`
	CreatedBy string    `json:"created_by"`
	Created   time.Time `json:"created"`
	// CapturedFrom is the Mac a set captured with lab set capture came from.
	CapturedFrom string `json:"captured_from,omitempty"`
	// Release and ManifestSHA256 are a Release's set, release-<version>:
	// its frozen manifest's packages, made from that manifest by its digest.
	Release        string    `json:"release,omitempty"`
	ManifestSHA256 string    `json:"manifest_sha256,omitempty"`
	Items          []SetItem `json:"items"`
}

// Where a Package set's package comes from.
const (
	SetRepo  = "repo"  // Ref is a version in the Mac's configured repositories
	SetFile  = "file"  // Ref is the sha256 of an uploaded package file
	SetBuild = "build" // Ref is a Builder build's id
)

// SetItem is one package of a Package set. Version is the package's version
// (pkgver-pkgrel) where known; SHA256 is the file's, for every file and
// build source, as labd resolved it when the set was made.
type SetItem struct {
	Package string `json:"package"`
	Source  string `json:"source"`
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	// File is a build's file the item resolved to: a package file's name,
	// or a kernel build's artifact.
	File string `json:"file,omitempty"`
}

// SetRequest makes a Package set (POST /api/sets).
type SetRequest struct {
	Name  string    `json:"name"`
	Items []SetItem `json:"items"`
}

// SetCapture makes a Package set of a Mac's installed stack, as its last
// check-in reported it (POST /api/sets/capture).
type SetCapture struct {
	Name string `json:"name"`
	Mac  string `json:"mac"`
}

var (
	reSetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	rePkgName = regexp.MustCompile(`^[a-z0-9@_+][a-z0-9@._+-]*$`)
	reSHA256  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CheckSetName says why a Package set's name will not do, if it won't.
func CheckSetName(name string) error {
	if !reSetName.MatchString(name) {
		return fmt.Errorf("a set's name is 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit: %q", name)
	}
	return nil
}

// ParseSetItem reads `NAME=repo:VERSION`, `NAME=file:SHA256[@VERSION]` or
// `NAME=build:ID`, as lab set create --pkg takes them.
func ParseSetItem(s string) (SetItem, error) {
	name, src, ok := strings.Cut(s, "=")
	kind, ref, ok2 := strings.Cut(src, ":")
	it := SetItem{Package: name, Source: kind, Ref: ref}
	if !ok || !ok2 {
		return it, fmt.Errorf("%q: want NAME=repo:VERSION, NAME=file:SHA256[@VERSION] or NAME=build:ID", s)
	}
	if kind == SetFile {
		it.Ref, it.Version, _ = strings.Cut(ref, "@")
	}
	if kind == SetRepo {
		it.Version = ref
	}
	return it, it.Check()
}

// Check says what is wrong with a set's item as asked for, if anything.
func (it SetItem) Check() error {
	if !rePkgName.MatchString(it.Package) {
		return fmt.Errorf("%q is not a package name", it.Package)
	}
	switch it.Source {
	case SetRepo, SetBuild:
		if it.Ref == "" {
			return fmt.Errorf("%s: %s needs a reference", it.Package, it.Source)
		}
	case SetFile:
		if !reSHA256.MatchString(it.Ref) {
			return fmt.Errorf("%s: file:%s is not a sha256 (lab upload prints it)", it.Package, it.Ref)
		}
	default:
		return fmt.Errorf("%s: source %q is none of repo, file, build", it.Package, it.Source)
	}
	return nil
}

// stackExact and stackPrefix name the packages of the Omarchy Mac stack that
// lab set capture records: the kernel and its headers, m1n1, U-Boot, Mesa
// and Vulkan, the AVD video decoder, libfprint and the Asahi firmware.
var (
	stackExact  = []string{"m1n1", "mesa", "libva-v4l2_request-avd", "libfprint", "asahi-fwextract"}
	stackPrefix = []string{"linux-aurora", "m1n1-", "uboot-", "mesa-", "vulkan-", "libfprint-"}
)

// bootedExact names, one by one, what a Performance run records it booted
// besides the stack, by Omarchy's own package names: the Neural engine's
// driver and libane's compiler, MLX, llama.cpp, the AVD firmware, FFmpeg
// and libva, and OpenCL. A name holding "ane" or "mlx" is no sign of either.
// They are not the stack: lab set capture leaves them out.
var bootedExact = []string{"omarchy-ane", "omarchy-ane-dkms", "mil-hwx-compiler", "omarchy-mlx", "omarchy-mlx-vulkan",
	"llama.cpp", "llama.cpp-vulkan", "avd-fw", "ffmpeg", "libva", "ocl-icd", "opencl-aurora"}

// BootedPackage is whether an installed package is one a Mac reports in its
// Facts: the stack, and what a Performance run records it booted.
func BootedPackage(name string) bool {
	return StackPackage(name) || slices.Contains(bootedExact, name)
}

// StackPackage is whether an installed package is part of the stack lab
// set capture records.
func StackPackage(name string) bool {
	for _, p := range stackExact {
		if name == p {
			return true
		}
	}
	for _, p := range stackPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return strings.Contains(name, "firmware") && strings.Contains(name, "asahi")
}
