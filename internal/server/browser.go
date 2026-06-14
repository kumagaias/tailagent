package server

import (
	"fmt"
	"os/exec"
	"runtime"
)

func openURLInBrowser(url string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
		args = []string{url}
	case "linux":
		command = "xdg-open"
		args = []string{url}
	case "windows":
		command = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		return fmt.Errorf("automatic browser opening is not supported on %s", runtime.GOOS)
	}
	return exec.Command(command, args...).Start()
}
