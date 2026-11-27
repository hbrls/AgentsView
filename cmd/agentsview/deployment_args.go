package main

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// deploymentArgs selects the image's command under AGENTSVIEW_MODE. It
// prefixes serve only when argv is empty or starts with a flag,
// so explicit subcommands and root help/version requests stay unchanged.
func deploymentArgs(args []string) ([]string, error) {
	if len(args) > 0 {
		if !strings.HasPrefix(args[0], "-") {
			return args, nil
		}
		name, _, _ := strings.Cut(args[0], "=")
		switch name {
		case "--help", "-h", "--version", "-v":
			return args, nil
		}
	}
	mode, set, err := config.DeploymentMode()
	if err != nil || !set {
		return args, err
	}
	if mode != config.DeploymentModeServe {
		return args, fmt.Errorf("unsupported AGENTSVIEW_MODE %q", mode)
	}
	return append([]string{"serve"}, args...), nil
}
