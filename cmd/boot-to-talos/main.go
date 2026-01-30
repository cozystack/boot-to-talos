//go:build linux

package main

import (
	"flag"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/cozystack/boot-to-talos/internal/boot"
	"github.com/cozystack/boot-to-talos/internal/cli"
	"github.com/cozystack/boot-to-talos/internal/install"
	"github.com/cozystack/boot-to-talos/internal/network"
	"github.com/cozystack/boot-to-talos/internal/source"
)

//nolint:gochecknoglobals
var (
	imageFlag string
	diskFlag  string
	modeFlag  string
)

func init() {
	flag.StringVar(&imageFlag, "image",
		"ghcr.io/cozystack/cozystack/talos:v1.11.6", "Talos installer image")
	flag.StringVar(&diskFlag, "disk", "", "target disk (will be wiped)")
	flag.BoolVar(&cli.YesFlag, "yes", false, "automatic yes to prompts")
	flag.StringVar(&modeFlag, "mode", "", "mode: boot or install")
}

func main() {
	var extra cli.MultiFlag
	sizeGiB := flag.Uint64("image-size-gib", 3, "image.raw size (GiB)")
	flag.Var(&extra, "extra-kernel-arg", "extra kernel arg (repeatable)")
	flag.Parse()

	// If mode is not specified, ask as first question
	if modeFlag == "" {
		modeFlag = cli.AskMode()
	} else {
		// Check validity of specified mode
		if modeFlag != "boot" && modeFlag != "install" {
			log.Fatalf("invalid mode: %s (must be 'boot' or 'install')", modeFlag)
		}
	}

	if imageFlag == flag.Lookup("image").DefValue {
		imageFlag = cli.Ask("Talos installer image", imageFlag)
	}

	// Detect image source type
	imgSource, err := source.DetectImageSource(imageFlag)
	if err != nil {
		log.Fatalf("failed to detect image source: %v", err)
	}
	defer imgSource.Close()

	// For install mode, ask for target disk after image selection
	if modeFlag == "install" && diskFlag == "" {
		def := firstDisk()
		if def == "" {
			diskFlag = cli.AskRequired("Target disk")
		} else {
			diskFlag = cli.Ask("Target disk", def)
		}
	}

	// Collect kernel args for both modes
	for _, e := range network.CollectKernelArgs() {
		extra = append(extra, e)
	}

	// Run selected mode
	if modeFlag == "boot" {
		boot.RunBootMode(imgSource, []string(extra))
		return
	}

	// Installation mode
	install.RunInstallMode(imgSource, diskFlag, []string(extra), *sizeGiB)
}

// firstDisk returns the first non-removable disk device.
func firstDisk() string {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		log.Printf("warning: failed to read /sys/block: %v", err)
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		// Skip virtual devices
		if strings.HasPrefix(name, "loop") ||
			strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "fd") ||
			strings.HasPrefix(name, "dm-") ||
			strings.HasPrefix(name, "sr") {
			continue
		}
		base := filepath.Join("/sys/block", name)
		if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
			log.Printf("debug: skipping %s: no device symlink", name)
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, "removable"))
		if err != nil {
			log.Printf("debug: skipping %s: cannot read removable flag: %v", name, err)
			continue
		}
		if strings.TrimSpace(string(b)) != "0" {
			log.Printf("debug: skipping %s: removable device", name)
			continue
		}
		return "/dev/" + name
	}
	log.Printf("warning: no suitable disk found")
	return ""
}

// Ensure fs.DirEntry is used (for os.ReadDir).
var _ fs.DirEntry = nil
