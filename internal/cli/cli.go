package cli

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
)

// MultiFlag allows a flag to be specified multiple times.
type MultiFlag []string

func (m *MultiFlag) String() string     { return strings.Join(*m, ",") }
func (m *MultiFlag) Set(v string) error { *m = append(*m, v); return nil }

//nolint:gochecknoglobals
var (
	// YesFlag enables automatic yes to prompts.
	YesFlag bool

	reader = bufio.NewReader(os.Stdin)
)

// Must logs a fatal error if err is not nil.
func Must(msg string, err error) {
	if err != nil {
		log.Fatalf("%s: %v", msg, err)
	}
}

// Ask prompts for input with a default value.
//
//nolint:forbidigo
func Ask(msg, def string) string {
	if YesFlag {
		fmt.Printf("%s [%s]: %s\n", msg, def, def)
		return def
	}
	fmt.Printf("%s [%s]: ", msg, def)
	t, _ := reader.ReadString('\n')
	t = strings.TrimSpace(t)
	if t == "" {
		return def
	}
	return t
}

// AskRequired prompts for required input (cannot be empty).
//
//nolint:forbidigo
func AskRequired(msg string) string {
	if YesFlag {
		log.Fatalf("missing required input for: %s (cannot auto-fill)", msg)
	}
	for {
		fmt.Printf("%s: ", msg)
		t, _ := reader.ReadString('\n')
		t = strings.TrimSpace(t)
		if t != "" {
			return t
		}
	}
}

// AskYesNo prompts for a yes/no answer with a default.
//
//nolint:forbidigo
func AskYesNo(msg string, def bool) bool {
	if YesFlag {
		fmt.Printf("%s [%s]: %v\n", msg, map[bool]string{true: "yes", false: "no"}[def], def)
		return def
	}
	defStr := "yes"
	if !def {
		defStr = "no"
	}
	for {
		fmt.Printf("%s [%s]: ", msg, defStr)
		in, _ := reader.ReadString('\n')
		in = strings.TrimSpace(strings.ToLower(in))
		if in == "" {
			return def
		}
		if in == "y" || in == "yes" {
			return true
		}
		if in == "n" || in == "no" {
			return false
		}
		fmt.Println("Please answer 'yes' or 'no'.")
	}
}

// AskMode prompts for boot/install mode selection.
//
//nolint:forbidigo
func AskMode() string {
	modeOptions := "Mode:\n" +
		"  1. boot – extract the kernel and initrd from the Talos installer and boot them directly using the kexec mechanism.\n" +
		"  2. install – prepare the environment, run the Talos installer, and then overwrite the system disk with the installed image."

	if YesFlag {
		fmt.Println(modeOptions)
		fmt.Println("Mode [1]: boot")
		return "boot"
	}
	for {
		fmt.Println(modeOptions)
		fmt.Print("Mode [1]: ")
		in, _ := reader.ReadString('\n')
		in = strings.TrimSpace(strings.ToLower(in))
		if in == "" || in == "1" || in == "boot" || in == "kexec" {
			return "boot"
		}
		if in == "2" || in == "install" {
			return "install"
		}
		fmt.Println("Please enter '1' or '2' (or 'boot'/'install').")
	}
}
