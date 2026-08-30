package toolchain

import (
	"bufio"
	"os"
	"strings"
)

// Per-distro guidance, and the rule that governs it (DESIGN section 6.5): a
// missing piece aborts the preflight "with per-distro guidance and NEVER a
// package-manager call".
//
// So this file is data and nothing else. It names the package that carries a
// tool on each distro family and the upstream documentation page, and the
// wizard renders both as text next to a Re-check button. Nothing in this
// package execs a package manager, asks for a password, or writes outside its
// own report — installing build tools is the user's decision on the user's
// machine, and a daemon that ran `sudo dnf install` on their behalf would be a
// far worse product than one that printed the line to copy.

// Family is the distro family a package name is chosen for. It is a family
// rather than a distro because the package names track the family: every
// Debian derivative calls the compiler bundle `build-essential`, and every
// Fedora derivative calls it `gcc-c++`.
type Family string

const (
	FamilyDebian  Family = "debian"
	FamilyFedora  Family = "fedora"
	FamilyArch    Family = "arch"
	FamilySUSE    Family = "suse"
	FamilyAlpine  Family = "alpine"
	FamilyGentoo  Family = "gentoo"
	FamilyUnknown Family = "unknown"
)

// FamilyValues lists the families this package has package names for, in the
// order the "on other distributions" fallback prefers them.
func FamilyValues() []Family {
	return []Family{FamilyDebian, FamilyFedora, FamilyArch, FamilySUSE, FamilyAlpine, FamilyGentoo}
}

// osReleaseIDs maps the `ID` and `ID_LIKE` values of /etc/os-release onto a
// family. Only IDs that actually change a package name are listed; everything
// else resolves through ID_LIKE, and an unknown host gets FamilyUnknown and the
// full table rather than a guess.
var osReleaseIDs = map[string]Family{
	"debian":              FamilyDebian,
	"ubuntu":              FamilyDebian,
	"linuxmint":           FamilyDebian,
	"pop":                 FamilyDebian,
	"raspbian":            FamilyDebian,
	"fedora":              FamilyFedora,
	"rhel":                FamilyFedora,
	"centos":              FamilyFedora,
	"rocky":               FamilyFedora,
	"almalinux":           FamilyFedora,
	"arch":                FamilyArch,
	"cachyos":             FamilyArch,
	"endeavouros":         FamilyArch,
	"manjaro":             FamilyArch,
	"opensuse":            FamilySUSE,
	"opensuse-leap":       FamilySUSE,
	"opensuse-tumbleweed": FamilySUSE,
	"sles":                FamilySUSE,
	"suse":                FamilySUSE,
	"alpine":              FamilyAlpine,
	"gentoo":              FamilyGentoo,
}

// DetectFamily reads an os-release file and returns the distro family it names.
// `path` empty reads /etc/os-release. An unreadable or unrecognized file is
// FamilyUnknown, which is a supported answer and not an error: the report then
// lists every family's package name instead of one.
func DetectFamily(path string) Family {
	if path == "" {
		path = "/etc/os-release"
	}
	f, err := os.Open(path)
	if err != nil {
		return FamilyUnknown
	}
	defer f.Close()

	var id string
	var like []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			id = strings.ToLower(value)
		case "ID_LIKE":
			like = strings.Fields(strings.ToLower(value))
		}
	}
	if fam, ok := osReleaseIDs[id]; ok {
		return fam
	}
	for _, l := range like {
		if fam, ok := osReleaseIDs[l]; ok {
			return fam
		}
	}
	return FamilyUnknown
}

// Guidance is what to tell a user about a tool they do not have.
type Guidance struct {
	// Packages is the package name per family. A family absent from the map has
	// no single package that carries the tool (nvcc on Alpine, say) and the
	// DocsURL is the whole answer there.
	Packages map[Family]string
	// DocsURL is the upstream page for the tool.
	DocsURL string
	// Extra is a sentence that applies regardless of distro.
	Extra string
}

// guidance is the whole table. Keys are the tool names below.
var guidance = map[string]Guidance{
	ToolGCC: {
		Packages: map[Family]string{
			FamilyDebian: "build-essential", FamilyFedora: "gcc",
			FamilyArch: "base-devel", FamilySUSE: "gcc", FamilyAlpine: "build-base",
			FamilyGentoo: "sys-devel/gcc",
		},
		DocsURL: "https://gcc.gnu.org/install/",
	},
	ToolGXX: {
		Packages: map[Family]string{
			FamilyDebian: "build-essential", FamilyFedora: "gcc-c++",
			FamilyArch: "base-devel", FamilySUSE: "gcc-c++", FamilyAlpine: "build-base",
			FamilyGentoo: "sys-devel/gcc",
		},
		DocsURL: "https://gcc.gnu.org/install/",
		Extra:   "llama.cpp is C++; a C compiler alone is not enough.",
	},
	ToolCMake: {
		Packages: map[Family]string{
			FamilyDebian: "cmake", FamilyFedora: "cmake", FamilyArch: "cmake",
			FamilySUSE: "cmake", FamilyAlpine: "cmake", FamilyGentoo: "dev-build/cmake",
		},
		DocsURL: "https://cmake.org/download/",
	},
	ToolNinja: {
		Packages: map[Family]string{
			FamilyDebian: "ninja-build", FamilyFedora: "ninja-build", FamilyArch: "ninja",
			FamilySUSE: "ninja", FamilyAlpine: "samurai", FamilyGentoo: "dev-build/ninja",
		},
		DocsURL: "https://ninja-build.org/",
		Extra:   "Optional: without it the build falls back to Unix Makefiles, which is slower but correct.",
	},
	ToolMake: {
		Packages: map[Family]string{
			FamilyDebian: "build-essential", FamilyFedora: "make", FamilyArch: "base-devel",
			FamilySUSE: "make", FamilyAlpine: "make", FamilyGentoo: "sys-devel/make",
		},
		DocsURL: "https://www.gnu.org/software/make/",
	},
	ToolGit: {
		Packages: map[Family]string{
			FamilyDebian: "git", FamilyFedora: "git", FamilyArch: "git",
			FamilySUSE: "git", FamilyAlpine: "git", FamilyGentoo: "dev-vcs/git",
		},
		DocsURL: "https://git-scm.com/downloads",
	},
	ToolCcache: {
		Packages: map[Family]string{
			FamilyDebian: "ccache", FamilyFedora: "ccache", FamilyArch: "ccache",
			FamilySUSE: "ccache", FamilyAlpine: "ccache", FamilyGentoo: "dev-util/ccache",
		},
		DocsURL: "https://ccache.dev/",
		Extra:   "Optional: it makes a rebuild of a nearby commit minutes rather than an hour.",
	},
	ToolNvcc: {
		Packages: map[Family]string{
			FamilyDebian: "nvidia-cuda-toolkit", FamilyFedora: "cuda-toolkit",
			FamilyArch: "cuda", FamilySUSE: "cuda-toolkit",
		},
		DocsURL: "https://docs.nvidia.com/cuda/cuda-installation-guide-linux/",
		Extra:   "Needed only for a CUDA build; a CPU build does not use it.",
	},
	ToolDriver: {
		DocsURL: "https://www.nvidia.com/download/index.aspx",
		Extra:   "The NVIDIA kernel driver ships nvidia-smi; a CUDA build needs both it and a GPU.",
	},
	ToolGlibc: {
		DocsURL: "https://www.gnu.org/software/libc/",
		Extra:   "Reported for the prebuilt acceptance check; there is nothing to install.",
	},
}

// GuidanceFor returns the guidance for a tool, and false when the tool has
// none — which is itself a fact worth rendering rather than hiding.
func GuidanceFor(tool string) (Guidance, bool) {
	g, ok := guidance[tool]
	return g, ok
}

// note renders the one-line hint the report carries for a tool that is absent
// or too old, for a given family.
func (g Guidance) note(fam Family, missing bool) string {
	var parts []string
	if missing {
		if pkg, ok := g.Packages[fam]; ok && fam != FamilyUnknown {
			parts = append(parts, "install the "+pkg+" package")
		} else if len(g.Packages) > 0 {
			var names []string
			for _, f := range FamilyValues() {
				if pkg, ok := g.Packages[f]; ok {
					names = append(names, string(f)+": "+pkg)
				}
			}
			parts = append(parts, "package name by distribution — "+strings.Join(names, ", "))
		}
	}
	if g.Extra != "" {
		parts = append(parts, g.Extra)
	}
	if g.DocsURL != "" {
		parts = append(parts, "see "+g.DocsURL)
	}
	return strings.Join(parts, "; ")
}
