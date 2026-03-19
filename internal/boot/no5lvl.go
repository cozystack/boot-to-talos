//go:build linux

package boot

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cozystack/boot-to-talos/internal/cli"
)

const grubDefaultPath = "/etc/default/grub"

// HandleNo5LVLWorkaround adds "no5lvl" to GRUB config and reboots
// to disable 5-level paging, which is incompatible with the Talos kernel
// (compiled without CONFIG_X86_5LEVEL).
//
// After the reboot the user must re-run boot-to-talos.
//
//nolint:forbidigo
func HandleNo5LVLWorkaround() {
	fmt.Println()
	fmt.Println("Host kernel uses 5-level page tables (LA57), which is incompatible")
	fmt.Println("with the Talos kernel. An intermediate reboot is required to disable")
	fmt.Println("5-level paging before Talos can be loaded.")
	fmt.Println()
	fmt.Println("This will NOT boot into Talos yet — it will:")
	fmt.Println("  1. Add 'no5lvl' to GRUB configuration")
	fmt.Println("  2. Reboot the current system with 5-level paging disabled")
	fmt.Println()
	fmt.Println("After reboot, run boot-to-talos again to load Talos.")
	fmt.Println()

	if !cli.AskYesNo("Reboot to disable 5-level paging?", true) {
		log.Fatal("aborted by user")
	}

	if err := patchGrubNo5LVL(); err != nil {
		log.Printf("error: %v", err)
		fallbackToManualInstructions()
	}

	if err := runUpdateGrub(); err != nil {
		log.Printf("error: %v", err)
		fallbackToManualInstructions()
	}

	log.Printf("rebooting to disable 5-level paging...")
	reboot()
}

// patchGrubNo5LVL adds "no5lvl" to GRUB_CMDLINE_LINUX in /etc/default/grub.
func patchGrubNo5LVL() error {
	data, err := os.ReadFile(grubDefaultPath)
	if err != nil {
		return errors.Wrapf(err, "read %s", grubDefaultPath)
	}

	content := string(data)

	if strings.Contains(content, "no5lvl") {
		log.Printf("'no5lvl' already present in %s", grubDefaultPath)
		return nil
	}

	patched, ok := addGrubCmdlineParam(content, "no5lvl")
	if !ok {
		return errors.Newf("GRUB_CMDLINE_LINUX not found in %s", grubDefaultPath)
	}

	if err := os.WriteFile(grubDefaultPath, []byte(patched), 0o644); err != nil {
		return errors.Wrapf(err, "write %s", grubDefaultPath)
	}

	log.Printf("added 'no5lvl' to %s", grubDefaultPath)

	return nil
}

// addGrubCmdlineParam appends a parameter to GRUB_CMDLINE_LINUX value.
// Returns the patched content and true if the line was found.
func addGrubCmdlineParam(content, param string) (string, bool) {
	var result strings.Builder
	found := false

	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !found && strings.HasPrefix(trimmed, "GRUB_CMDLINE_LINUX=") {
			// Find the closing quote and insert param before it.
			lastQuote := strings.LastIndex(line, "\"")
			if lastQuote > 0 {
				result.WriteString(line[:lastQuote])
				result.WriteString(" " + param)
				result.WriteString(line[lastQuote:])
				found = true
			} else {
				result.WriteString(line)
			}
		} else {
			result.WriteString(line)
		}
		result.WriteString("\n")
	}

	// Remove trailing extra newline (SplitSeq produces empty last element).
	out := result.String()
	if strings.HasSuffix(content, "\n") {
		out = strings.TrimSuffix(out, "\n")
	} else {
		out = strings.TrimSuffix(out, "\n")
	}

	return out, found
}

// runUpdateGrub runs update-grub to regenerate GRUB config.
func runUpdateGrub() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "update-grub") //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "update-grub")
	}

	return nil
}

// reboot triggers a normal system reboot.
func reboot() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "reboot") //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Fatalf("reboot failed: %v", err)
	}
}

// fallbackToManualInstructions prints manual workaround steps and exits.
//
//nolint:forbidigo
func fallbackToManualInstructions() {
	fmt.Println()
	log.Fatal("Automatic workaround failed. Manual steps:\n" +
		"  1. Edit /etc/default/grub: add 'no5lvl' to GRUB_CMDLINE_LINUX\n" +
		"  2. Run: update-grub && reboot\n" +
		"  3. Re-run boot-to-talos")
}
